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
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1api "k8s.io/api/apps/v1"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientTesting "k8s.io/client-go/testing"
	clientFake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/vmware-tanzu/velero/pkg/builder"
	"github.com/vmware-tanzu/velero/pkg/util/kube"
)

type reactor struct {
	verb        string
	resource    string
	reactorFunc clientTesting.ReactionFunc
}

func TestIsRunning(t *testing.T) {
	ds := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		kubeReactors  []reactor
		expectErr     string
	}{
		{
			name:      "ds is not found",
			namespace: "fake-ns",
			expectErr: "daemonset not found",
		},
		{
			name:      "ds get error",
			namespace: "fake-ns",
			kubeReactors: []reactor{
				{
					verb:     "get",
					resource: "daemonsets",
					reactorFunc: func(action clientTesting.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("fake-get-error")
					},
				},
			},
			expectErr: "fake-get-error",
		},
		{
			name:      "succeed",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				ds,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			for _, reactor := range test.kubeReactors {
				fakeKubeClient.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorFunc)
			}

			err := isRunning(t.Context(), fakeKubeClient, test.namespace, daemonSet)
			if test.expectErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestIsRunningInNode(t *testing.T) {
	scheme := runtime.NewScheme()
	corev1api.AddToScheme(scheme)

	nonNodeAgentPod := builder.ForPod("fake-ns", "fake-pod").Result()
	nodeAgentPodNotRunning := builder.ForPod("fake-ns", "fake-pod").Labels(map[string]string{"role": "node-agent"}).Result()
	nodeAgentPodRunning1 := builder.ForPod("fake-ns", "fake-pod-1").Labels(map[string]string{"role": "node-agent"}).Phase(corev1api.PodRunning).Result()
	nodeAgentPodRunning2 := builder.ForPod("fake-ns", "fake-pod-2").Labels(map[string]string{"role": "node-agent"}).Phase(corev1api.PodRunning).Result()
	nodeAgentPodRunning3 := builder.ForPod("fake-ns", "fake-pod-3").
		Labels(map[string]string{"role": "node-agent"}).
		Phase(corev1api.PodRunning).
		NodeName("fake-node").
		Result()

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		nodeName      string
		expectErr     string
	}{
		{
			name:      "node name is empty",
			expectErr: "node name is empty",
		},
		{
			name:     "ds pod not found",
			nodeName: "fake-node",
			kubeClientObj: []runtime.Object{
				nonNodeAgentPod,
			},
			expectErr: "daemonset pod not found in running state in node fake-node",
		},
		{
			name:     "ds po are not all running",
			nodeName: "fake-node",
			kubeClientObj: []runtime.Object{
				nodeAgentPodNotRunning,
				nodeAgentPodRunning1,
			},
			expectErr: "daemonset pod not found in running state in node fake-node",
		},
		{
			name:     "ds pods wrong node name",
			nodeName: "fake-node",
			kubeClientObj: []runtime.Object{
				nodeAgentPodNotRunning,
				nodeAgentPodRunning1,
				nodeAgentPodRunning2,
			},
			expectErr: "daemonset pod not found in running state in node fake-node",
		},
		{
			name:     "succeed",
			nodeName: "fake-node",
			kubeClientObj: []runtime.Object{
				nodeAgentPodNotRunning,
				nodeAgentPodRunning1,
				nodeAgentPodRunning2,
				nodeAgentPodRunning3,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeClientBuilder := clientFake.NewClientBuilder()
			fakeClientBuilder = fakeClientBuilder.WithScheme(scheme)

			fakeClient := fakeClientBuilder.WithRuntimeObjects(test.kubeClientObj...).Build()

			err := IsRunningInNode(t.Context(), "", test.nodeName, fakeClient)
			if test.expectErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetPodSpec(t *testing.T) {
	podSpec := corev1api.PodSpec{
		NodeName: "fake-node",
	}

	daemonSet := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: podSpec,
			},
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		expectErr     string
		expectSpec    corev1api.PodSpec
	}{
		{
			name:      "ds is not found",
			namespace: "fake-ns",
			expectErr: "error to get node-agent daemonset: daemonsets.apps \"node-agent\" not found",
		},
		{
			name:      "succeed",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectSpec: podSpec,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			spec, err := GetPodSpec(t.Context(), fakeKubeClient, test.namespace, kube.NodeOSLinux)
			if test.expectErr == "" {
				require.NoError(t, err)
				assert.Equal(t, *spec, test.expectSpec)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetConfigs(t *testing.T) {
	cm := builder.ForConfigMap("fake-ns", "node-agent-config").Result()
	cmWithInvalidDataFormat := builder.ForConfigMap("fake-ns", "node-agent-config").Data("fake-key", "wrong").Result()
	cmWithoutCocurrentData := builder.ForConfigMap("fake-ns", "node-agent-config").Data("fake-key", "{\"someothers\":{\"someother\": 10}}").Result()
	cmWithValidData := builder.ForConfigMap("fake-ns", "node-agent-config").Data("fake-key", "{\"loadConcurrency\":{\"globalConfig\": 5}}").Result()

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		kubeReactors  []reactor
		expectResult  *Configs
		expectErr     string
	}{
		{
			name:      "cm get error",
			namespace: "fake-ns",
			kubeReactors: []reactor{
				{
					verb:     "get",
					resource: "configmaps",
					reactorFunc: func(action clientTesting.Action) (handled bool, ret runtime.Object, err error) {
						return true, nil, errors.New("fake-get-error")
					},
				},
			},
			expectErr: "error to get node agent configs node-agent-config: fake-get-error",
		},
		{
			name:      "cm's data is nil",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				cm,
			},
			expectErr: "data is not available in config map node-agent-config",
		},
		{
			name:      "cm's data is with invalid format",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				cmWithInvalidDataFormat,
			},
			expectErr: "error to unmarshall configs from node-agent-config: invalid character 'w' looking for beginning of value",
		},
		{
			name:      "concurrency configs are not found",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				cmWithoutCocurrentData,
			},
			expectResult: &Configs{},
		},
		{
			name:      "success",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				cmWithValidData,
			},
			expectResult: &Configs{
				LoadConcurrency: &LoadConcurrency{
					GlobalConfig: 5,
				},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			for _, reactor := range test.kubeReactors {
				fakeKubeClient.Fake.PrependReactor(reactor.verb, reactor.resource, reactor.reactorFunc)
			}

			result, err := GetConfigs(t.Context(), test.namespace, fakeKubeClient, "node-agent-config")
			if test.expectErr == "" {
				require.NoError(t, err)

				if test.expectResult == nil {
					assert.Nil(t, result)
				} else if test.expectResult.LoadConcurrency == nil {
					assert.Nil(t, result.LoadConcurrency)
				} else {
					assert.Equal(t, *test.expectResult.LoadConcurrency, *result.LoadConcurrency)
				}
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetLabelValue(t *testing.T) {
	daemonSet := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
	}

	daemonSetWithOtherLabel := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"fake-other-label": "fake-value-1",
					},
				},
			},
		},
	}

	daemonSetWithLabel := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"fake-label": "fake-value-2",
					},
				},
			},
		},
	}

	daemonSetWithEmptyLabel := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"fake-label": "",
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		expectedValue string
		expectErr     string
	}{
		{
			name:      "ds get error",
			namespace: "fake-ns",
			expectErr: "error getting node-agent daemonset: daemonsets.apps \"node-agent\" not found",
		},
		{
			name:      "no label",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectErr: ErrNodeAgentLabelNotFound.Error(),
		},
		{
			name:      "no expecting label",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithOtherLabel,
			},
			expectErr: ErrNodeAgentLabelNotFound.Error(),
		},
		{
			name:      "expecting label",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithLabel,
			},
			expectedValue: "fake-value-2",
		},
		{
			name:      "expecting empty label",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithEmptyLabel,
			},
			expectedValue: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			value, err := GetLabelValue(t.Context(), fakeKubeClient, test.namespace, "fake-label", kube.NodeOSLinux)
			if test.expectErr == "" {
				require.NoError(t, err)
				assert.Equal(t, test.expectedValue, value)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetAnnotationValue(t *testing.T) {
	daemonSet := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
	}

	daemonSetWithOtherAnnotation := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"fake-other-annotation": "fake-value-1",
					},
				},
			},
		},
	}

	daemonSetWithAnnotation := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"fake-annotation": "fake-value-2",
					},
				},
			},
		},
	}

	daemonSetWithEmptyAnnotation := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"fake-annotation": "",
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		expectedValue string
		expectErr     string
	}{
		{
			name:      "ds get error",
			namespace: "fake-ns",
			expectErr: "error getting node-agent daemonset: daemonsets.apps \"node-agent\" not found",
		},
		{
			name:      "no annotation",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectErr: ErrNodeAgentAnnotationNotFound.Error(),
		},
		{
			name:      "no expecting annotation",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithOtherAnnotation,
			},
			expectErr: ErrNodeAgentAnnotationNotFound.Error(),
		},
		{
			name:      "expecting annotation",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithAnnotation,
			},
			expectedValue: "fake-value-2",
		},
		{
			name:      "expecting empty annotation",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithEmptyAnnotation,
			},
			expectedValue: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			value, err := GetAnnotationValue(t.Context(), fakeKubeClient, test.namespace, "fake-annotation", kube.NodeOSLinux)
			if test.expectErr == "" {
				require.NoError(t, err)
				assert.Equal(t, test.expectedValue, value)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetToleration(t *testing.T) {
	daemonSet := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
	}

	daemonSetWithOtherToleration := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: corev1api.PodSpec{
					Tolerations: []corev1api.Toleration{
						{
							Key: "other-toleration-key",
						},
					},
				},
			},
		},
	}

	daemonSetWithToleration := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: corev1api.PodSpec{
					Tolerations: []corev1api.Toleration{
						{
							Key:   "fake-toleration",
							Value: "true",
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		expectedValue corev1api.Toleration
		expectErr     string
	}{
		// {
		// 	name:      "ds get error",
		// 	namespace: "fake-ns",
		// 	expectErr: "error getting node-agent daemonset: daemonsets.apps \"node-agent\" not found",
		// },
		{
			name:      "no toleration",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectErr: ErrNodeAgentTolerationNotFound.Error(),
		},
		{
			name:      "no expecting toleration",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithOtherToleration,
			},
			expectErr: ErrNodeAgentTolerationNotFound.Error(),
		},
		{
			name:      "expecting toleration",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithToleration,
			},
			expectedValue: corev1api.Toleration{
				Key:   "fake-toleration",
				Value: "true",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			value, err := GetToleration(t.Context(), fakeKubeClient, test.namespace, "fake-toleration", kube.NodeOSLinux)
			if test.expectErr == "" {
				require.NoError(t, err)
				assert.Equal(t, test.expectedValue, *value)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}

func TestGetHostPodPath(t *testing.T) {
	daemonSet := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
	}

	daemonSetWithHostPodVolume := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: corev1api.PodSpec{
					Volumes: []corev1api.Volume{
						{
							Name: HostPodVolumeMount,
						},
					},
				},
			},
		},
	}

	daemonSetWithHostPodVolumeAndEmptyPath := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: corev1api.PodSpec{
					Volumes: []corev1api.Volume{
						{
							Name: HostPodVolumeMount,
							VolumeSource: corev1api.VolumeSource{
								HostPath: &corev1api.HostPathVolumeSource{},
							},
						},
					},
				},
			},
		},
	}

	daemonSetWithHostPodVolumeAndValidPath := &appsv1api.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "fake-ns",
			Name:      "node-agent",
		},
		TypeMeta: metav1.TypeMeta{
			Kind: "DaemonSet",
		},
		Spec: appsv1api.DaemonSetSpec{
			Template: corev1api.PodTemplateSpec{
				Spec: corev1api.PodSpec{
					Volumes: []corev1api.Volume{
						{
							Name: HostPodVolumeMount,
							VolumeSource: corev1api.VolumeSource{
								HostPath: &corev1api.HostPathVolumeSource{
									Path: "/var/lib/kubelet/pods",
								},
							},
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		kubeClientObj []runtime.Object
		namespace     string
		osType        string
		expectedValue string
		expectErr     string
	}{
		{
			name:      "ds get error",
			namespace: "fake-ns",
			osType:    kube.NodeOSWindows,
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectErr: "error getting daemonset node-agent-windows: daemonsets.apps \"node-agent-windows\" not found",
		},
		{
			name:      "no host pod volume",
			namespace: "fake-ns",
			osType:    kube.NodeOSLinux,
			kubeClientObj: []runtime.Object{
				daemonSet,
			},
			expectErr: "host pod volume is not found",
		},
		{
			name:      "no host pod volume path",
			namespace: "fake-ns",
			osType:    kube.NodeOSLinux,
			kubeClientObj: []runtime.Object{
				daemonSetWithHostPodVolume,
			},
			expectErr: "host pod volume is not a host path volume",
		},
		{
			name:      "empty host pod volume path",
			namespace: "fake-ns",
			osType:    kube.NodeOSLinux,
			kubeClientObj: []runtime.Object{
				daemonSetWithHostPodVolumeAndEmptyPath,
			},
			expectErr: "host pod volume path is empty",
		},
		{
			name:      "succeed",
			namespace: "fake-ns",
			osType:    kube.NodeOSLinux,
			kubeClientObj: []runtime.Object{
				daemonSetWithHostPodVolumeAndValidPath,
			},
			expectedValue: "/var/lib/kubelet/pods",
		},
		{
			name:      "succeed on empty os type",
			namespace: "fake-ns",
			kubeClientObj: []runtime.Object{
				daemonSetWithHostPodVolumeAndValidPath,
			},
			expectedValue: "/var/lib/kubelet/pods",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeKubeClient := fake.NewSimpleClientset(test.kubeClientObj...)

			path, err := GetHostPodPath(t.Context(), fakeKubeClient, test.namespace, test.osType)

			if test.expectErr == "" {
				require.NoError(t, err)
				assert.Equal(t, test.expectedValue, path)
			} else {
				assert.EqualError(t, err, test.expectErr)
			}
		})
	}
}
