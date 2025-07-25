/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodeagent

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bombsimon/logrusr/v3"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	corev1api "k8s.io/api/core/v1"
	storagev1api "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	snapshotv1client "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"

	velerov1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	velerov2alpha1api "github.com/vmware-tanzu/velero/pkg/apis/velero/v2alpha1"
	"github.com/vmware-tanzu/velero/pkg/buildinfo"
	"github.com/vmware-tanzu/velero/pkg/client"
	"github.com/vmware-tanzu/velero/pkg/cmd"
	"github.com/vmware-tanzu/velero/pkg/cmd/util/signals"
	"github.com/vmware-tanzu/velero/pkg/constant"
	"github.com/vmware-tanzu/velero/pkg/controller"
	"github.com/vmware-tanzu/velero/pkg/datapath"
	"github.com/vmware-tanzu/velero/pkg/exposer"
	"github.com/vmware-tanzu/velero/pkg/metrics"
	"github.com/vmware-tanzu/velero/pkg/nodeagent"
	"github.com/vmware-tanzu/velero/pkg/util/filesystem"
	"github.com/vmware-tanzu/velero/pkg/util/kube"
	"github.com/vmware-tanzu/velero/pkg/util/logging"

	cacheutil "k8s.io/client-go/tools/cache"
)

var (
	scheme = runtime.NewScheme()
)

const (
	// the port where prometheus metrics are exposed
	defaultMetricsAddress = ":8085"

	defaultResourceTimeout         = 10 * time.Minute
	defaultDataMoverPrepareTimeout = 30 * time.Minute
	defaultDataPathConcurrentNum   = 1
)

type nodeAgentServerConfig struct {
	metricsAddress          string
	resourceTimeout         time.Duration
	dataMoverPrepareTimeout time.Duration
	nodeAgentConfig         string
}

func NewServerCommand(f client.Factory) *cobra.Command {
	logLevelFlag := logging.LogLevelFlag(logrus.InfoLevel)
	formatFlag := logging.NewFormatFlag()
	config := nodeAgentServerConfig{
		metricsAddress:          defaultMetricsAddress,
		resourceTimeout:         defaultResourceTimeout,
		dataMoverPrepareTimeout: defaultDataMoverPrepareTimeout,
	}

	command := &cobra.Command{
		Use:    "server",
		Short:  "Run the velero node-agent server",
		Long:   "Run the velero node-agent server",
		Hidden: true,
		Run: func(c *cobra.Command, args []string) {
			logLevel := logLevelFlag.Parse()
			logrus.Infof("Setting log-level to %s", strings.ToUpper(logLevel.String()))

			logger := logging.DefaultMergeLogger(logLevel, formatFlag.Parse())
			logger.Infof("Starting Velero node-agent server %s (%s)", buildinfo.Version, buildinfo.FormattedGitSHA())

			f.SetBasename(fmt.Sprintf("%s-%s", c.Parent().Name(), c.Name()))
			s, err := newNodeAgentServer(logger, f, config)
			cmd.CheckError(err)

			s.run()
		},
	}

	command.Flags().Var(logLevelFlag, "log-level", fmt.Sprintf("The level at which to log. Valid values are %s.", strings.Join(logLevelFlag.AllowedValues(), ", ")))
	command.Flags().Var(formatFlag, "log-format", fmt.Sprintf("The format for log output. Valid values are %s.", strings.Join(formatFlag.AllowedValues(), ", ")))
	command.Flags().DurationVar(&config.resourceTimeout, "resource-timeout", config.resourceTimeout, "How long to wait for resource processes which are not covered by other specific timeout parameters. Default is 10 minutes.")
	command.Flags().DurationVar(&config.dataMoverPrepareTimeout, "data-mover-prepare-timeout", config.dataMoverPrepareTimeout, "How long to wait for preparing a DataUpload/DataDownload. Default is 30 minutes.")
	command.Flags().StringVar(&config.metricsAddress, "metrics-address", config.metricsAddress, "The address to expose prometheus metrics")
	command.Flags().StringVar(&config.nodeAgentConfig, "node-agent-configmap", config.nodeAgentConfig, "The name of ConfigMap containing node-agent configurations.")

	return command
}

type nodeAgentServer struct {
	logger            logrus.FieldLogger
	ctx               context.Context
	cancelFunc        context.CancelFunc
	fileSystem        filesystem.Interface
	mgr               manager.Manager
	metrics           *metrics.ServerMetrics
	metricsAddress    string
	namespace         string
	nodeName          string
	config            nodeAgentServerConfig
	kubeClient        kubernetes.Interface
	csiSnapshotClient *snapshotv1client.Clientset
	dataPathMgr       *datapath.Manager
	dataPathConfigs   *nodeagent.Configs
	vgdpCounter       *exposer.VgdpCounter
}

func newNodeAgentServer(logger logrus.FieldLogger, factory client.Factory, config nodeAgentServerConfig) (*nodeAgentServer, error) {
	ctx, cancelFunc := context.WithCancel(context.Background())

	clientConfig, err := factory.ClientConfig()
	if err != nil {
		cancelFunc()
		return nil, err
	}

	ctrl.SetLogger(logrusr.New(logger))
	klog.SetLogger(logrusr.New(logger)) // klog.Logger is used by k8s.io/client-go

	if err := velerov1api.AddToScheme(scheme); err != nil {
		cancelFunc()
		return nil, err
	}
	if err := velerov2alpha1api.AddToScheme(scheme); err != nil {
		cancelFunc()
		return nil, err
	}
	if err := corev1api.AddToScheme(scheme); err != nil {
		cancelFunc()
		return nil, err
	}
	if err := storagev1api.AddToScheme(scheme); err != nil {
		cancelFunc()
		return nil, err
	}

	nodeName := os.Getenv("NODE_NAME")

	// use a field selector to filter to only pods scheduled on this node.
	cacheOption := cache.Options{
		ByObject: map[ctrlclient.Object]cache.ByObject{
			&corev1api.Pod{}: {
				Field: fields.Set{"spec.nodeName": nodeName}.AsSelector(),
			},
			&velerov1api.PodVolumeBackup{}: {
				Field: fields.Set{"metadata.namespace": factory.Namespace()}.AsSelector(),
			},
			&velerov1api.PodVolumeRestore{}: {
				Field: fields.Set{"metadata.namespace": factory.Namespace()}.AsSelector(),
			},
			&velerov2alpha1api.DataUpload{}: {
				Field: fields.Set{"metadata.namespace": factory.Namespace()}.AsSelector(),
			},
			&velerov2alpha1api.DataDownload{}: {
				Field: fields.Set{"metadata.namespace": factory.Namespace()}.AsSelector(),
			},
			&corev1api.Event{}: {
				Field: fields.Set{"metadata.namespace": factory.Namespace()}.AsSelector(),
			},
		},
	}

	var mgr manager.Manager
	retry := 10
	for {
		mgr, err = ctrl.NewManager(clientConfig, ctrl.Options{
			Scheme: scheme,
			Cache:  cacheOption,
		})
		if err == nil {
			break
		}

		retry--
		if retry == 0 {
			break
		}

		logger.WithError(err).Warn("Failed to create controller manager, need retry")

		time.Sleep(time.Second)
	}

	if err != nil {
		cancelFunc()
		return nil, errors.Wrap(err, "error creating controller manager")
	}

	s := &nodeAgentServer{
		logger:         logger,
		ctx:            ctx,
		cancelFunc:     cancelFunc,
		fileSystem:     filesystem.NewFileSystem(),
		mgr:            mgr,
		config:         config,
		namespace:      factory.Namespace(),
		nodeName:       nodeName,
		metricsAddress: config.metricsAddress,
	}

	// the cache isn't initialized yet when "validatePodVolumesHostPath" is called, the client returned by the manager cannot
	// be used, so we need the kube client here
	s.kubeClient, err = factory.KubeClient()
	if err != nil {
		return nil, err
	}
	if err := s.validatePodVolumesHostPath(s.kubeClient); err != nil {
		return nil, err
	}

	s.csiSnapshotClient, err = snapshotv1client.NewForConfig(clientConfig)
	if err != nil {
		return nil, err
	}

	s.getDataPathConfigs()
	s.dataPathMgr = datapath.NewManager(s.getDataPathConcurrentNum(defaultDataPathConcurrentNum))

	return s, nil
}

func (s *nodeAgentServer) run() {
	signals.CancelOnShutdown(s.cancelFunc, s.logger)

	go func() {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", promhttp.Handler())
		s.logger.Infof("Starting metric server for node agent at address [%s]", s.metricsAddress)
		server := &http.Server{
			Addr:              s.metricsAddress,
			Handler:           metricsMux,
			ReadHeaderTimeout: 3 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil {
			s.logger.Fatalf("Failed to start metric server for node agent at [%s]: %v", s.metricsAddress, err)
		}
	}()
	s.metrics = metrics.NewNodeMetrics()
	s.metrics.RegisterAllMetrics()
	s.metrics.InitMetricsForNode(s.nodeName)

	s.logger.Info("Starting controllers")

	var loadAffinity []*kube.LoadAffinity
	if s.dataPathConfigs != nil && len(s.dataPathConfigs.LoadAffinity) > 0 {
		loadAffinity = s.dataPathConfigs.LoadAffinity
		s.logger.Infof("Using customized loadAffinity %v", loadAffinity)
	}

	var backupPVCConfig map[string]nodeagent.BackupPVC
	if s.dataPathConfigs != nil && s.dataPathConfigs.BackupPVCConfig != nil {
		backupPVCConfig = s.dataPathConfigs.BackupPVCConfig
		s.logger.Infof("Using customized backupPVC config %v", backupPVCConfig)
	}

	podResources := corev1api.ResourceRequirements{}
	if s.dataPathConfigs != nil && s.dataPathConfigs.PodResources != nil {
		if res, err := kube.ParseResourceRequirements(s.dataPathConfigs.PodResources.CPURequest, s.dataPathConfigs.PodResources.MemoryRequest, s.dataPathConfigs.PodResources.CPULimit, s.dataPathConfigs.PodResources.MemoryLimit); err != nil {
			s.logger.WithError(err).Warn("Pod resource requirements are invalid, ignore")
		} else {
			podResources = res
			s.logger.Infof("Using customized pod resource requirements %v", s.dataPathConfigs.PodResources)
		}
	}

	if s.dataPathConfigs != nil && s.dataPathConfigs.LoadConcurrency != nil && s.dataPathConfigs.LoadConcurrency.PrepareQueueLength > 0 {
		if counter, err := exposer.StartVgdpCounter(s.ctx, s.mgr, s.dataPathConfigs.LoadConcurrency.PrepareQueueLength); err != nil {
			s.logger.WithError(err).Warnf("Failed to start VGDP counter, VDGP loads are not constrained")
		} else {
			s.vgdpCounter = counter
			s.logger.Infof("VGDP loads are constrained with %d", s.dataPathConfigs.LoadConcurrency.PrepareQueueLength)
		}
	}

	pvbReconciler := controller.NewPodVolumeBackupReconciler(s.mgr.GetClient(), s.mgr, s.kubeClient, s.dataPathMgr, s.vgdpCounter, s.nodeName, s.config.dataMoverPrepareTimeout, s.config.resourceTimeout, podResources, s.metrics, s.logger)
	if err := pvbReconciler.SetupWithManager(s.mgr); err != nil {
		s.logger.Fatal(err, "unable to create controller", "controller", constant.ControllerPodVolumeBackup)
	}

	pvrReconciler := controller.NewPodVolumeRestoreReconciler(s.mgr.GetClient(), s.mgr, s.kubeClient, s.dataPathMgr, s.vgdpCounter, s.nodeName, s.config.dataMoverPrepareTimeout, s.config.resourceTimeout, podResources, s.logger)
	if err := pvrReconciler.SetupWithManager(s.mgr); err != nil {
		s.logger.WithError(err).Fatal("Unable to create the pod volume restore controller")
	}

	if err := controller.InitLegacyPodVolumeRestoreReconciler(s.mgr.GetClient(), s.mgr, s.kubeClient, s.dataPathMgr, s.namespace, s.config.resourceTimeout, s.logger); err != nil {
		s.logger.WithError(err).Fatal("Unable to create the legacy pod volume restore controller")
	}

	dataUploadReconciler := controller.NewDataUploadReconciler(
		s.mgr.GetClient(),
		s.mgr,
		s.kubeClient,
		s.csiSnapshotClient.SnapshotV1(),
		s.dataPathMgr,
		s.vgdpCounter,
		loadAffinity,
		backupPVCConfig,
		podResources,
		clock.RealClock{},
		s.nodeName,
		s.config.dataMoverPrepareTimeout,
		s.logger,
		s.metrics,
	)
	if err := dataUploadReconciler.SetupWithManager(s.mgr); err != nil {
		s.logger.WithError(err).Fatal("Unable to create the data upload controller")
	}

	var restorePVCConfig nodeagent.RestorePVC
	if s.dataPathConfigs != nil && s.dataPathConfigs.RestorePVCConfig != nil {
		restorePVCConfig = *s.dataPathConfigs.RestorePVCConfig
		s.logger.Infof("Using customized restorePVC config %v", restorePVCConfig)
	}

	dataDownloadReconciler := controller.NewDataDownloadReconciler(
		s.mgr.GetClient(),
		s.mgr,
		s.kubeClient,
		s.dataPathMgr,
		s.vgdpCounter,
		loadAffinity,
		restorePVCConfig,
		podResources,
		s.nodeName,
		s.config.dataMoverPrepareTimeout,
		s.logger,
		s.metrics,
	)

	if err := dataDownloadReconciler.SetupWithManager(s.mgr); err != nil {
		s.logger.WithError(err).Fatal("Unable to create the data download controller")
	}

	go func() {
		if err := s.waitCacheForResume(); err != nil {
			s.logger.WithError(err).Error("Failed to wait cache for resume, will not resume DU/DD")
			return
		}

		if err := dataUploadReconciler.AttemptDataUploadResume(s.ctx, s.logger.WithField("node", s.nodeName), s.namespace); err != nil {
			s.logger.WithError(errors.WithStack(err)).Error("Failed to attempt data upload resume")
		}

		if err := dataDownloadReconciler.AttemptDataDownloadResume(s.ctx, s.logger.WithField("node", s.nodeName), s.namespace); err != nil {
			s.logger.WithError(errors.WithStack(err)).Error("Failed to attempt data download resume")
		}

		if err := pvbReconciler.AttemptPVBResume(s.ctx, s.logger.WithField("node", s.nodeName), s.namespace); err != nil {
			s.logger.WithError(errors.WithStack(err)).Error("Failed to attempt PVB resume")
		}

		if err := pvrReconciler.AttemptPVRResume(s.ctx, s.logger.WithField("node", s.nodeName), s.namespace); err != nil {
			s.logger.WithError(errors.WithStack(err)).Error("Failed to attempt PVR resume")
		}

		s.markLegacyPVRsFailed(s.mgr.GetClient())
	}()

	s.logger.Info("Controllers starting...")

	if err := s.mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		s.logger.Fatal("Problem starting manager", err)
	}
}

func (s *nodeAgentServer) waitCacheForResume() error {
	podInformer, err := s.mgr.GetCache().GetInformer(s.ctx, &corev1api.Pod{})
	if err != nil {
		return errors.Wrap(err, "error getting pod informer")
	}

	duInformer, err := s.mgr.GetCache().GetInformer(s.ctx, &velerov2alpha1api.DataUpload{})
	if err != nil {
		return errors.Wrap(err, "error getting du informer")
	}

	ddInformer, err := s.mgr.GetCache().GetInformer(s.ctx, &velerov2alpha1api.DataDownload{})
	if err != nil {
		return errors.Wrap(err, "error getting dd informer")
	}

	pvbInformer, err := s.mgr.GetCache().GetInformer(s.ctx, &velerov1api.PodVolumeBackup{})
	if err != nil {
		return errors.Wrap(err, "error getting PVB informer")
	}

	pvrInformer, err := s.mgr.GetCache().GetInformer(s.ctx, &velerov1api.PodVolumeRestore{})
	if err != nil {
		return errors.Wrap(err, "error getting PVR informer")
	}

	if !cacheutil.WaitForCacheSync(s.ctx.Done(), podInformer.HasSynced, duInformer.HasSynced, ddInformer.HasSynced, pvbInformer.HasSynced, pvrInformer.HasSynced) {
		return errors.New("error waiting informer synced")
	}

	return nil
}

// validatePodVolumesHostPath validates that the pod volumes path contains a
// directory for each Pod running on this node
func (s *nodeAgentServer) validatePodVolumesHostPath(client kubernetes.Interface) error {
	files, err := s.fileSystem.ReadDir(nodeagent.HostPodVolumeMountPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.logger.Warnf("Pod volumes host path [%s] doesn't exist, fs-backup is disabled", nodeagent.HostPodVolumeMountPath())
			return nil
		}
		return errors.Wrap(err, "could not read pod volumes host path")
	}

	// create a map of directory names inside the pod volumes path
	dirs := sets.NewString()
	for _, f := range files {
		if f.IsDir() {
			dirs.Insert(f.Name())
		}
	}

	pods, err := client.CoreV1().Pods("").List(s.ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("spec.nodeName=%s,status.phase=Running", s.nodeName)})
	if err != nil {
		return errors.WithStack(err)
	}

	valid := true
	for _, pod := range pods.Items {
		dirName := string(pod.GetUID())

		// if the pod is a mirror pod, the directory name is the hash value of the
		// mirror pod annotation
		if hash, ok := pod.GetAnnotations()[corev1api.MirrorPodAnnotationKey]; ok {
			dirName = hash
		}

		if !dirs.Has(dirName) {
			valid = false
			s.logger.WithFields(logrus.Fields{
				"pod":  fmt.Sprintf("%s/%s", pod.GetNamespace(), pod.GetName()),
				"path": nodeagent.HostPodVolumeMountPath() + "/" + dirName,
			}).Debug("could not find volumes for pod in host path")
		}
	}

	if !valid {
		return errors.New("unexpected directory structure for host-pods volume, ensure that the host-pods volume corresponds to the pods subdirectory of the kubelet root directory")
	}

	return nil
}

func (s *nodeAgentServer) markLegacyPVRsFailed(client ctrlclient.Client) {
	pvrs := &velerov1api.PodVolumeRestoreList{}
	if err := client.List(s.ctx, pvrs, &ctrlclient.ListOptions{Namespace: s.namespace}); err != nil {
		s.logger.WithError(errors.WithStack(err)).Error("failed to list podvolumerestores")
		return
	}

	for i, pvr := range pvrs.Items {
		if !controller.IsLegacyPVR(&pvr) {
			continue
		}

		if pvr.Status.Phase != velerov1api.PodVolumeRestorePhaseInProgress {
			s.logger.Debugf("the status of podvolumerestore %q is %q, skip", pvr.GetName(), pvr.Status.Phase)
			continue
		}

		pod := &corev1api.Pod{}
		if err := client.Get(s.ctx, types.NamespacedName{
			Namespace: pvr.Spec.Pod.Namespace,
			Name:      pvr.Spec.Pod.Name,
		}, pod); err != nil {
			s.logger.WithError(errors.WithStack(err)).Errorf("failed to get pod \"%s/%s\" of podvolumerestore %q",
				pvr.Spec.Pod.Namespace, pvr.Spec.Pod.Name, pvr.GetName())
			continue
		}
		if pod.Spec.NodeName != s.nodeName {
			s.logger.Debugf("the node of pod referenced by podvolumerestore %q is %q, not %q, skip", pvr.GetName(), pod.Spec.NodeName, s.nodeName)
			continue
		}

		if err := controller.UpdatePVRStatusToFailed(s.ctx, client, &pvrs.Items[i], errors.New("cannot survive from node-agent restart"),
			fmt.Sprintf("get a legacy podvolumerestore with status %q during the server starting, mark it as %q", velerov1api.PodVolumeRestorePhaseInProgress, velerov1api.PodVolumeRestorePhaseFailed),
			time.Now(), s.logger); err != nil {
			s.logger.WithError(errors.WithStack(err)).Errorf("failed to patch podvolumerestore %q", pvr.GetName())
			continue
		}
		s.logger.WithField("podvolumerestore", pvr.GetName()).Warn(pvr.Status.Message)
	}
}

var getConfigsFunc = nodeagent.GetConfigs

func (s *nodeAgentServer) getDataPathConfigs() {
	if s.config.nodeAgentConfig == "" {
		s.logger.Info("No node-agent configMap is specified")
		return
	}

	configs, err := getConfigsFunc(s.ctx, s.namespace, s.kubeClient, s.config.nodeAgentConfig)
	if err != nil {
		s.logger.WithError(err).Warnf("Failed to get node agent configs from configMap %s, ignore it", s.config.nodeAgentConfig)
		return
	}

	s.dataPathConfigs = configs
}

func (s *nodeAgentServer) getDataPathConcurrentNum(defaultNum int) int {
	configs := s.dataPathConfigs

	if configs == nil || configs.LoadConcurrency == nil {
		s.logger.Infof("Concurrency configs are not found, use the default number %v", defaultNum)
		return defaultNum
	}

	globalNum := configs.LoadConcurrency.GlobalConfig

	if globalNum <= 0 {
		s.logger.Warnf("Global number %v is invalid, use the default value %v", globalNum, defaultNum)
		globalNum = defaultNum
	}

	if len(configs.LoadConcurrency.PerNodeConfig) == 0 {
		return globalNum
	}

	curNode, err := s.kubeClient.CoreV1().Nodes().Get(s.ctx, s.nodeName, metav1.GetOptions{})
	if err != nil {
		s.logger.WithError(err).Warnf("Failed to get node info for %s, use the global number %v", s.nodeName, globalNum)
		return globalNum
	}

	concurrentNum := math.MaxInt32

	for _, rule := range configs.LoadConcurrency.PerNodeConfig {
		selector, err := metav1.LabelSelectorAsSelector(&(rule.NodeSelector))
		if err != nil {
			s.logger.WithError(err).Warnf("Failed to parse rule with label selector %s, skip it", rule.NodeSelector.String())
			continue
		}

		if rule.Number <= 0 {
			s.logger.Warnf("Rule with label selector %s is with an invalid number %v, skip it", rule.NodeSelector.String(), rule.Number)
			continue
		}

		if selector.Matches(labels.Set(curNode.GetLabels())) {
			if concurrentNum > rule.Number {
				concurrentNum = rule.Number
			}
		}
	}

	if concurrentNum == math.MaxInt32 {
		s.logger.Infof("Per node number for node %s is not found, use the global number %v", s.nodeName, globalNum)
		concurrentNum = globalNum
	} else {
		s.logger.Infof("Use the per node number %v over global number %v for node %s", concurrentNum, globalNum, s.nodeName)
	}

	return concurrentNum
}
