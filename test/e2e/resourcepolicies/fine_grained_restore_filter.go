/*
Copyright the Velero contributors.

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

package resourcepolicies

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	velerov1api "github.com/velero-io/velero/pkg/apis/velero/v1"
	. "github.com/velero-io/velero/test/e2e/test"
	. "github.com/velero-io/velero/test/util/k8s"
	. "github.com/velero-io/velero/test/util/velero"
)

// FineGrainedRestoreFilter tests the end-to-end restore-side filtering capability:
// 1. Fine-grained per-namespace, per-kind filtering in namespacedFilterPolicies
// 2. Fine-grained global cluster-scoped filtering in clusterScopedFilterPolicy
//
// In this scenario:
// - A full/unfiltered backup captures all test resources (ConfigMaps, Secrets, ClusterRoles) into the archive.
// - At restore time, RestoreSpec.ResourcePolicy references a ConfigMap containing:
//   - namespacedFilterPolicies (refines restore for the target namespace)
//   - clusterScopedFilterPolicy (refines restore for cluster-scoped resources)
//
// Verification:
// - Namespace-scoped:
//   - ConfigMap matching kind, labelSelector, and names pattern is RESTORED.
//   - ConfigMap matching excludedNames ("*-debug") is EXCLUDED at restore time.
//   - ConfigMap with mismatched labelSelector is EXCLUDED at restore time.
//   - Secret is EXCLUDED at restore time because kind Secret is not listed in resourceFilters (exclusive allowlist).
//
// - Cluster-scoped:
//   - ClusterRole matching kind, labelSelector, and names pattern is RESTORED.
//   - ClusterRole matching excludedNames ("*-debug") is EXCLUDED at restore time.
//   - ClusterRole with mismatched labelSelector is EXCLUDED at restore time.
//   - ClusterRole with unmatched names pattern is EXCLUDED at restore time.
type FineGrainedRestoreFilter struct {
	TestCase
	cmName            string
	testNS            string
	clusterLabelKey   string
	clusterLabelValue string

	crIncluded        string
	crDebug           string
	crMismatchedLabel string
	crUnmatchedName   string
	clusterRoles      []string
}

var FineGrainedRestoreFilterTest func() = TestFunc(&FineGrainedRestoreFilter{})

func (r *FineGrainedRestoreFilter) Init() error {
	Expect(r.TestCase.Init()).To(Succeed())

	r.CaseBaseName = "fg-restore-filter-" + r.UUIDgen
	r.BackupName = "backup-" + r.CaseBaseName
	r.RestoreName = "restore-" + r.CaseBaseName
	r.cmName = "cm-" + r.CaseBaseName

	r.testNS = r.CaseBaseName + "-ns"
	r.clusterLabelKey = "app"
	r.clusterLabelValue = "cluster-workload-" + r.UUIDgen

	r.crIncluded = fmt.Sprintf("cr-%s-included", r.UUIDgen)
	r.crDebug = fmt.Sprintf("cr-%s-debug", r.UUIDgen)
	r.crMismatchedLabel = fmt.Sprintf("cr-%s-mismatched-label", r.UUIDgen)
	r.crUnmatchedName = fmt.Sprintf("other-%s-role", r.UUIDgen)
	r.clusterRoles = []string{r.crIncluded, r.crDebug, r.crMismatchedLabel, r.crUnmatchedName}

	r.TestMsg = &TestMSG{
		Desc:      "Integration test for fine-grained restore filters via RestoreSpec.ResourcePolicy",
		Text:      "should apply namespacedFilterPolicies and clusterScopedFilterPolicy at restore time to selectively restore from a backup archive",
		FailedMSG: "Failed to perform selective restore with fine-grained restore filters",
	}

	// Backup includes the test namespace and cluster resources without filter policies
	r.BackupArgs = []string{
		"create", "--namespace", r.VeleroCfg.VeleroNamespace, "backup", r.BackupName,
		"--include-namespaces", r.testNS,
		"--include-cluster-resources=true",
		"--wait",
	}

	// Restore specifies the fine-grained filter policy ConfigMap
	r.RestoreArgs = []string{
		"create", "--namespace", r.VeleroCfg.VeleroNamespace, "restore", r.RestoreName,
		"--from-backup", r.BackupName,
		"--resource-policies-configmap", r.cmName,
		"--wait",
	}

	return nil
}

func (r *FineGrainedRestoreFilter) CreateResources() error {
	// Restore policy ConfigMap contains only clusterScopedFilterPolicy and namespacedFilterPolicies
	// (includeExcludePolicy is not valid on restore).
	yamlConfig := fmt.Sprintf(`version: v1
clusterScopedFilterPolicy:
  resourceFilters:
    - kinds: ["ClusterRole"]
      labelSelector:
        matchLabels:
          %s: "%s"
      names: ["cr-%s-*"]
      excludedNames: ["*-debug"]
namespacedFilterPolicies:
  - namespaces:
      - "%s"
    resourceFilters:
      - kinds: ["ConfigMap"]
        labelSelector:
          matchLabels:
            app: test-workload
        names: ["app-*"]
        excludedNames: ["*-debug"]
`, r.clusterLabelKey, r.clusterLabelValue, r.UUIDgen, r.testNS)

	By(fmt.Sprintf("Create restore ResourcePolicy ConfigMap %s in namespace %s\n", r.cmName, r.VeleroCfg.VeleroNamespace), func() {
		Expect(CreateConfigMapFromYAMLData(r.Client.ClientGo, yamlConfig, r.cmName, r.VeleroCfg.VeleroNamespace)).To(Succeed(),
			fmt.Sprintf("Failed to create configmap %s in namespace %s", r.cmName, r.VeleroCfg.VeleroNamespace))
	})
	By(fmt.Sprintf("Waiting for ConfigMap %s in namespace %s to be ready\n", r.cmName, r.VeleroCfg.VeleroNamespace), func() {
		Expect(WaitForConfigMapComplete(r.Client.ClientGo, r.VeleroCfg.VeleroNamespace, r.cmName)).To(Succeed(),
			fmt.Sprintf("Failed to wait for configmap %s in namespace %s ready", r.cmName, r.VeleroCfg.VeleroNamespace))
	})

	// 1. Create test namespace
	By(fmt.Sprintf("Create test namespace %s\n", r.testNS), func() {
		Expect(CreateNamespaceWithLabel(r.Ctx, r.Client, r.testNS, map[string]string{})).To(Succeed(),
			fmt.Sprintf("Failed to create test namespace %s", r.testNS))
	})

	// Inside test namespace:
	// a. ConfigMap to be RESTORED: matches kind (ConfigMap), label (app=test-workload), name (app-*)
	By("Create matching ConfigMap in test namespace", func() {
		_, err := CreateConfigMap(r.Client.ClientGo, r.testNS, "app-cfg-included", map[string]string{"app": "test-workload"}, map[string]string{"data": "retained"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(r.Client.ClientGo, r.testNS, "app-cfg-included")).To(Succeed())
	})

	// b. ConfigMap to be EXCLUDED by name: matches label, but name matches excludedNames (*-debug)
	By("Create debug ConfigMap in test namespace (to be excluded by restore excludedNames)", func() {
		_, err := CreateConfigMap(r.Client.ClientGo, r.testNS, "app-cfg-debug", map[string]string{"app": "test-workload"}, map[string]string{"data": "debug"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(r.Client.ClientGo, r.testNS, "app-cfg-debug")).To(Succeed())
	})

	// c. ConfigMap to be EXCLUDED by label: mismatched label (app=other)
	By("Create ConfigMap with mismatched label in test namespace (to be excluded by restore labelSelector)", func() {
		_, err := CreateConfigMap(r.Client.ClientGo, r.testNS, "other-cfg", map[string]string{"app": "other"}, map[string]string{"data": "other"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(r.Client.ClientGo, r.testNS, "other-cfg")).To(Succeed())
	})

	// d. Secret to be EXCLUDED by kind: kind Secret is not listed in namespacedFilterPolicies
	By("Create Secret in test namespace (to be excluded by restore kind allowlist)", func() {
		_, err := CreateSecret(r.Client.ClientGo, r.testNS, "app-secret", map[string]string{"app": "test-workload"})
		Expect(err).To(Succeed())
		Expect(WaitForSecretsComplete(r.Client.ClientGo, r.testNS, "app-secret")).To(Succeed())
	})

	// 2. Create ClusterRoles:
	// a. Matching ClusterRole: matches kind (ClusterRole), labelSelector, and name pattern (cr-<UUID>-*)
	By(fmt.Sprintf("Create matching ClusterRole %s", r.crIncluded), func() {
		Expect(r.createTestClusterRole(r.crIncluded, map[string]string{r.clusterLabelKey: r.clusterLabelValue})).To(Succeed())
	})

	// b. Excluded ClusterRole by name: matches label, but matches excludedNames (*-debug)
	By(fmt.Sprintf("Create debug ClusterRole %s (to be excluded by restore excludedNames)", r.crDebug), func() {
		Expect(r.createTestClusterRole(r.crDebug, map[string]string{r.clusterLabelKey: r.clusterLabelValue})).To(Succeed())
	})

	// c. Excluded ClusterRole by label: mismatched label
	By(fmt.Sprintf("Create ClusterRole %s with mismatched label (to be excluded by restore labelSelector)", r.crMismatchedLabel), func() {
		Expect(r.createTestClusterRole(r.crMismatchedLabel, map[string]string{r.clusterLabelKey: "other"})).To(Succeed())
	})

	// d. Excluded ClusterRole by name pattern: name does not match cr-<UUID>-*
	By(fmt.Sprintf("Create ClusterRole %s with unmatched name pattern", r.crUnmatchedName), func() {
		Expect(r.createTestClusterRole(r.crUnmatchedName, map[string]string{r.clusterLabelKey: r.clusterLabelValue})).To(Succeed())
	})

	return nil
}

func (r *FineGrainedRestoreFilter) createTestClusterRole(name string, labels map[string]string) error {
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"configmaps"},
				Verbs:     []string{"get", "list"},
			},
		},
	}
	_, err := r.Client.ClientGo.RbacV1().ClusterRoles().Create(r.Ctx, role, metav1.CreateOptions{})
	return err
}

func (r *FineGrainedRestoreFilter) Backup() error {
	veleroCfg := r.GetTestCase().VeleroCfg

	By(fmt.Sprintf("Start backup %s covering namespace and cluster-scoped resources...", r.BackupName), func() {
		Expect(VeleroCmdExec(r.Ctx, veleroCfg.VeleroCLI, r.BackupArgs)).To(Succeed(), "Failed to create backup")
	})

	By(fmt.Sprintf("Waiting for backup %s to complete\n", r.BackupName), func() {
		Expect(wait.PollUntilContextTimeout(r.Ctx, PollInterval, PollTimeout, true, func(ctx context.Context) (bool, error) {
			got := &velerov1api.Backup{}
			if err := r.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: r.VeleroCfg.VeleroNamespace, Name: r.BackupName}, got); err != nil {
				return false, err
			}
			switch got.Status.Phase {
			case velerov1api.BackupPhaseCompleted:
				return true, nil
			case velerov1api.BackupPhaseFailed, velerov1api.BackupPhasePartiallyFailed,
				velerov1api.BackupPhaseFailedValidation:
				return false, errors.Newf("backup %s ended in phase %s", r.BackupName, got.Status.Phase)
			default:
				return false, nil
			}
		})).To(Succeed(), fmt.Sprintf("Backup %s did not complete successfully", r.BackupName))
	})

	return nil
}

func (r *FineGrainedRestoreFilter) Destroy() error {
	By("Deleting test cluster roles before restore", func() {
		for _, cr := range r.clusterRoles {
			_ = r.Client.ClientGo.RbacV1().ClusterRoles().Delete(r.Ctx, cr, metav1.DeleteOptions{})
		}
	})
	return r.TestCase.Destroy()
}

func (r *FineGrainedRestoreFilter) Restore() error {
	veleroCfg := r.GetTestCase().VeleroCfg

	By(fmt.Sprintf("Start restore %s with resource policies configmap %s...", r.RestoreName, r.cmName), func() {
		Expect(VeleroCmdExec(r.Ctx, veleroCfg.VeleroCLI, r.RestoreArgs)).To(Succeed(), "Failed to create restore")
	})

	By(fmt.Sprintf("Waiting for restore %s to complete\n", r.RestoreName), func() {
		Expect(wait.PollUntilContextTimeout(r.Ctx, PollInterval, PollTimeout, true, func(ctx context.Context) (bool, error) {
			got := &velerov1api.Restore{}
			if err := r.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: r.VeleroCfg.VeleroNamespace, Name: r.RestoreName}, got); err != nil {
				return false, err
			}
			switch got.Status.Phase {
			case velerov1api.RestorePhaseCompleted:
				return true, nil
			case velerov1api.RestorePhaseFailed, velerov1api.RestorePhasePartiallyFailed,
				velerov1api.RestorePhaseFailedValidation:
				return false, errors.Newf("restore %s ended in phase %s", r.RestoreName, got.Status.Phase)
			default:
				return false, nil
			}
		})).To(Succeed(), fmt.Sprintf("Restore %s did not complete successfully", r.RestoreName))
	})

	return nil
}

func (r *FineGrainedRestoreFilter) Verify() error {
	// 1. Verify test namespace was restored
	By(fmt.Sprintf("Verify namespace %s was restored", r.testNS), func() {
		_, err := GetNamespace(r.Ctx, r.Client, r.testNS)
		Expect(err).To(Succeed(), fmt.Sprintf("Namespace %s should exist after restore", r.testNS))
	})

	// 2. Verify matching ConfigMap was restored
	By("Verify matching ConfigMap app-cfg-included was restored", func() {
		cm, err := GetConfigMap(r.Client.ClientGo, r.testNS, "app-cfg-included")
		Expect(err).To(Succeed(), "app-cfg-included should exist in restored namespace")
		Expect(cm.Data["data"]).To(Equal("retained"))
	})

	// 3. Verify debug ConfigMap was excluded at restore time by excludedNames
	By("Verify ConfigMap app-cfg-debug was NOT restored (excluded by restore excludedNames)", func() {
		_, err := GetConfigMap(r.Client.ClientGo, r.testNS, "app-cfg-debug")
		Expect(err).To(HaveOccurred(), "app-cfg-debug should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "app-cfg-debug error should be NotFound")
	})

	// 4. Verify ConfigMap with mismatched label was excluded at restore time by labelSelector
	By("Verify ConfigMap other-cfg was NOT restored (excluded by restore labelSelector)", func() {
		_, err := GetConfigMap(r.Client.ClientGo, r.testNS, "other-cfg")
		Expect(err).To(HaveOccurred(), "other-cfg should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "other-cfg error should be NotFound")
	})

	// 5. Verify Secret was excluded at restore time by kind allowlist
	By("Verify Secret app-secret was NOT restored (excluded by restore kind allowlist)", func() {
		_, err := GetSecret(r.Client.ClientGo, r.testNS, "app-secret")
		Expect(err).To(HaveOccurred(), "app-secret should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "app-secret error should be NotFound")
	})

	// 6. Verify clusterScopedFilterPolicy for ClusterRoles at restore time:
	// a. Matching ClusterRole must be restored
	By(fmt.Sprintf("Verify matching ClusterRole %s was restored", r.crIncluded), func() {
		cr, err := GetClusterRole(r.Ctx, r.Client, r.crIncluded)
		Expect(err).To(Succeed(), fmt.Sprintf("ClusterRole %s should be restored", r.crIncluded))
		Expect(cr.Name).To(Equal(r.crIncluded))
	})

	// b. Debug ClusterRole must NOT be restored (excluded by restore excludedNames)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by restore clusterScopedFilterPolicy excludedNames)", r.crDebug), func() {
		_, err := GetClusterRole(r.Ctx, r.Client, r.crDebug)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", r.crDebug))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	// c. ClusterRole with mismatched label must NOT be restored (excluded by restore labelSelector)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by restore clusterScopedFilterPolicy labelSelector)", r.crMismatchedLabel), func() {
		_, err := GetClusterRole(r.Ctx, r.Client, r.crMismatchedLabel)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", r.crMismatchedLabel))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	// d. ClusterRole with unmatched name pattern must NOT be restored (excluded by restore names pattern)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by restore clusterScopedFilterPolicy names pattern)", r.crUnmatchedName), func() {
		_, err := GetClusterRole(r.Ctx, r.Client, r.crUnmatchedName)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", r.crUnmatchedName))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	return nil
}

func (r *FineGrainedRestoreFilter) Clean() error {
	if CurrentSpecReport().Failed() && r.VeleroCfg.FailFast {
		fmt.Println("Test case failed and fail fast is enabled. Skip resource clean up.")
		return nil
	}

	for _, cr := range r.clusterRoles {
		_ = r.Client.ClientGo.RbacV1().ClusterRoles().Delete(r.Ctx, cr, metav1.DeleteOptions{})
	}

	if err := DeleteConfigMap(r.Client.ClientGo, r.VeleroCfg.VeleroNamespace, r.cmName); err != nil {
		return errors.Wrap(err, "failed to delete resource policy configmap")
	}

	return r.GetTestCase().Clean()
}
