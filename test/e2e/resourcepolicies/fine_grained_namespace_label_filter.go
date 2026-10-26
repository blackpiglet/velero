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
	"github.com/velero-io/velero/pkg/builder"
	. "github.com/velero-io/velero/test/e2e/test"
	. "github.com/velero-io/velero/test/util/k8s"
)

// FineGrainedNamespaceLabelFilter tests the end-to-end integration between:
// 1. Dynamic namespace selection by label in includeExcludePolicy (includedNamespacesByLabel)
// 2. Global resource filters in includeExcludePolicy (includedNamespaceScopedResources,
//    excludedNamespaceScopedResources, and includedClusterScopedResources)
// 3. Fine-grained per-namespace, per-kind filtering in namespacedFilterPolicies
// 4. Fine-grained global cluster-scoped filtering in clusterScopedFilterPolicy
//
// In this scenario:
// - A ResourcePolicy ConfigMap specifies all four layers:
//   - includeExcludePolicy (namespace label selector + global resource includes/excludes)
//   - clusterScopedFilterPolicy (fine-grained filters for cluster-scoped resources)
//   - namespacedFilterPolicies (fine-grained filters for namespaced resources)
//
// - Namespaces:
//   - Namespaces matching includedNamespacesByLabel are selected; untargeted namespaces are excluded.
//
// - Within targeted namespace:
//   - ConfigMap matching global inclusion, fine-grained label, and name pattern is INCLUDED and restored.
//   - ConfigMap matching name pattern "*-debug" is EXCLUDED by fine-grained excludedNames.
//   - ConfigMap not matching labelSelector is EXCLUDED by fine-grained labelSelector.
//   - Secret is listed in namespacedFilterPolicies, but EXCLUDED because global resource filter
//     (excludedNamespaceScopedResources) overrides fine-grained policy.
//
// - In untargeted namespaces:
//   - Resources matching all fine-grained filter criteria are still NOT backed up because
//     the entire namespace was excluded by includedNamespacesByLabel.
//
// - Cluster-scoped resources:
//   - ClusterRole matching global inclusion (includedClusterScopedResources), fine-grained kind,
//     label selector, and name pattern is INCLUDED and restored.
//   - ClusterRole matching name pattern "*-debug" is EXCLUDED by clusterScopedFilterPolicy excludedNames.
//   - ClusterRole with mismatched label is EXCLUDED by clusterScopedFilterPolicy labelSelector.
//   - ClusterRole with unmatched name pattern is EXCLUDED by clusterScopedFilterPolicy names.
type FineGrainedNamespaceLabelFilter struct {
	TestCase
	cmName            string
	labelKey          string
	labelValue        string
	clusterLabelKey   string
	clusterLabelValue string
	targetedNS        string
	untargetedNS      string

	crIncluded        string
	crDebug           string
	crMismatchedLabel string
	crUnmatchedName   string
	clusterRoles      []string
}

var FineGrainedNamespaceLabelFilterTest func() = TestFunc(&FineGrainedNamespaceLabelFilter{})

func (f *FineGrainedNamespaceLabelFilter) Init() error {
	Expect(f.TestCase.Init()).To(Succeed())

	f.CaseBaseName = "fg-ns-filter-" + f.UUIDgen
	f.BackupName = "backup-" + f.CaseBaseName
	f.RestoreName = "restore-" + f.CaseBaseName
	f.cmName = "cm-" + f.CaseBaseName
	f.labelKey = "velero-e2e-selector"
	f.labelValue = "target-" + f.UUIDgen

	f.clusterLabelKey = "app"
	f.clusterLabelValue = "cluster-workload-" + f.UUIDgen

	f.targetedNS = f.CaseBaseName + "-targeted"
	f.untargetedNS = f.CaseBaseName + "-untargeted"

	f.crIncluded = fmt.Sprintf("cr-%s-included", f.UUIDgen)
	f.crDebug = fmt.Sprintf("cr-%s-debug", f.UUIDgen)
	f.crMismatchedLabel = fmt.Sprintf("cr-%s-mismatched-label", f.UUIDgen)
	f.crUnmatchedName = fmt.Sprintf("other-%s-role", f.UUIDgen)
	f.clusterRoles = []string{f.crIncluded, f.crDebug, f.crMismatchedLabel, f.crUnmatchedName}

	f.TestMsg = &TestMSG{
		Desc:      "Integration test for fine-grained filters with namespace label selector, global resource filters, and clusterScopedFilterPolicy via ResourcePolicy",
		Text:      "should back up only namespaces matching includedNamespacesByLabel, respect global resource filters, and apply namespacedFilterPolicies and clusterScopedFilterPolicy",
		FailedMSG: "Failed to perform backup and restore with fine-grained filters, namespace label selector, global resource filters, and clusterScopedFilterPolicy",
	}

	// Backup is created directly via the API with no IncludedNamespaces set to allow
	// includedNamespacesByLabel to replace the wildcard baseline.
	// Restore is performed without resource policy configmap since includeExcludePolicy
	// is not valid on restore.
	f.RestoreArgs = []string{
		"create", "--namespace", f.VeleroCfg.VeleroNamespace, "restore", f.RestoreName,
		"--from-backup", f.BackupName, "--wait",
	}

	return nil
}

func (f *FineGrainedNamespaceLabelFilter) CreateResources() error {
	// The ResourcePolicy ConfigMap combines:
	// 1. Namespace label selector: selects only namespaces labeled velero-e2e-selector=target-<UUID>
	// 2. Global resource filters:
	//    - includedNamespaceScopedResources: explicitly includes configmaps
	//    - excludedNamespaceScopedResources: globally excludes secrets
	//    - includedClusterScopedResources: explicitly includes clusterroles
	// 3. ClusterScopedFilterPolicy:
	//    - filters ClusterRoles globally by label and name glob patterns (cr-<UUID>-*, excluding *-debug)
	// 4. NamespacedFilterPolicies:
	//    - targets the selected namespace
	//    - lists both ConfigMap and Secret under kinds
	//    - filters by label (app=test-workload) and name glob patterns (app-*, excluding *-debug)
	yamlConfig := fmt.Sprintf(`version: v1
includeExcludePolicy:
  includedNamespacesByLabel:
    - "%s=%s"
  includedNamespaceScopedResources:
    - "configmaps"
  excludedNamespaceScopedResources:
    - "secrets"
  includedClusterScopedResources:
    - "clusterroles"
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
      - kinds: ["ConfigMap", "Secret"]
        labelSelector:
          matchLabels:
            app: test-workload
        names: ["app-*"]
        excludedNames: ["*-debug"]
`, f.labelKey, f.labelValue, f.clusterLabelKey, f.clusterLabelValue, f.UUIDgen, f.targetedNS)

	By(fmt.Sprintf("Create ResourcePolicy ConfigMap %s in namespace %s\n", f.cmName, f.VeleroCfg.VeleroNamespace), func() {
		Expect(CreateConfigMapFromYAMLData(f.Client.ClientGo, yamlConfig, f.cmName, f.VeleroCfg.VeleroNamespace)).To(Succeed(),
			fmt.Sprintf("Failed to create configmap %s in namespace %s", f.cmName, f.VeleroCfg.VeleroNamespace))
	})
	By(fmt.Sprintf("Waiting for ConfigMap %s in namespace %s to be ready\n", f.cmName, f.VeleroCfg.VeleroNamespace), func() {
		Expect(WaitForConfigMapComplete(f.Client.ClientGo, f.VeleroCfg.VeleroNamespace, f.cmName)).To(Succeed(),
			fmt.Sprintf("Failed to wait for configmap %s in namespace %s ready", f.cmName, f.VeleroCfg.VeleroNamespace))
	})

	// 1. Create targeted namespace with label
	By(fmt.Sprintf("Create targeted namespace %s with label %s=%s\n", f.targetedNS, f.labelKey, f.labelValue), func() {
		Expect(CreateNamespaceWithLabel(f.Ctx, f.Client, f.targetedNS, map[string]string{f.labelKey: f.labelValue})).To(Succeed(),
			fmt.Sprintf("Failed to create targeted namespace %s", f.targetedNS))
	})

	// Inside targeted namespace:
	// a. Target ConfigMap to be INCLUDED: matches global filter (configmaps), fine-grained kind (ConfigMap),
	//    fine-grained label (app=test-workload), and name pattern (app-*)
	By("Create matching ConfigMap in targeted namespace", func() {
		_, err := CreateConfigMap(f.Client.ClientGo, f.targetedNS, "app-cfg-included", map[string]string{"app": "test-workload"}, map[string]string{"data": "retained"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(f.Client.ClientGo, f.targetedNS, "app-cfg-included")).To(Succeed())
	})

	// b. Excluded ConfigMap by name: matches global filter and label, but matches fine-grained excludedNames (*-debug)
	By("Create debug ConfigMap in targeted namespace (to be excluded by fine-grained name filter)", func() {
		_, err := CreateConfigMap(f.Client.ClientGo, f.targetedNS, "app-cfg-debug", map[string]string{"app": "test-workload"}, map[string]string{"data": "debug"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(f.Client.ClientGo, f.targetedNS, "app-cfg-debug")).To(Succeed())
	})

	// c. Excluded ConfigMap by label: matches global filter, but has mismatched label (app=other)
	By("Create ConfigMap with mismatched label in targeted namespace (to be excluded by fine-grained label filter)", func() {
		_, err := CreateConfigMap(f.Client.ClientGo, f.targetedNS, "other-cfg", map[string]string{"app": "other"}, map[string]string{"data": "other"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(f.Client.ClientGo, f.targetedNS, "other-cfg")).To(Succeed())
	})

	// d. Excluded Secret: listed in namespacedFilterPolicies and matches fine-grained label & name,
	//    but globally EXCLUDED by excludedNamespaceScopedResources: ["secrets"]
	By("Create Secret in targeted namespace (matches fine-grained filter but excluded by global resource filter)", func() {
		_, err := CreateSecret(f.Client.ClientGo, f.targetedNS, "app-secret", map[string]string{"app": "test-workload"})
		Expect(err).To(Succeed())
		Expect(WaitForSecretsComplete(f.Client.ClientGo, f.targetedNS, "app-secret")).To(Succeed())
	})

	// 2. Create untargeted namespace without matching label
	By(fmt.Sprintf("Create untargeted namespace %s\n", f.untargetedNS), func() {
		Expect(CreateNamespaceWithLabel(f.Ctx, f.Client, f.untargetedNS, map[string]string{})).To(Succeed(),
			fmt.Sprintf("Failed to create untargeted namespace %s", f.untargetedNS))
	})

	// In untargeted namespace: create a ConfigMap that would match both global and fine-grained filters,
	// but must be excluded because the namespace itself was not targeted by includedNamespacesByLabel.
	By("Create ConfigMap in untargeted namespace", func() {
		_, err := CreateConfigMap(f.Client.ClientGo, f.untargetedNS, "app-cfg-included", map[string]string{"app": "test-workload"}, map[string]string{"data": "should-not-backup"})
		Expect(err).To(Succeed())
		Expect(WaitForConfigMapComplete(f.Client.ClientGo, f.untargetedNS, "app-cfg-included")).To(Succeed())
	})

	// 3. Create Cluster-Scoped Resources (ClusterRoles) for clusterScopedFilterPolicy verification:
	// a. Matching ClusterRole: matches global include (clusterroles), kind (ClusterRole),
	//    labelSelector (app=cluster-workload-<UUID>), and name pattern (cr-<UUID>-*)
	By(fmt.Sprintf("Create matching ClusterRole %s", f.crIncluded), func() {
		Expect(f.createTestClusterRole(f.crIncluded, map[string]string{f.clusterLabelKey: f.clusterLabelValue})).To(Succeed())
	})

	// b. Excluded ClusterRole by name: matches label and name pattern, but matches excludedNames (*-debug)
	By(fmt.Sprintf("Create debug ClusterRole %s (to be excluded by excludedNames)", f.crDebug), func() {
		Expect(f.createTestClusterRole(f.crDebug, map[string]string{f.clusterLabelKey: f.clusterLabelValue})).To(Succeed())
	})

	// c. Excluded ClusterRole by label: matches name pattern, but has mismatched label
	By(fmt.Sprintf("Create ClusterRole %s with mismatched label (to be excluded by labelSelector)", f.crMismatchedLabel), func() {
		Expect(f.createTestClusterRole(f.crMismatchedLabel, map[string]string{f.clusterLabelKey: "other"})).To(Succeed())
	})

	// d. Excluded ClusterRole by name pattern: matches label, but name does not match cr-<UUID>-*
	By(fmt.Sprintf("Create ClusterRole %s with unmatched name pattern", f.crUnmatchedName), func() {
		Expect(f.createTestClusterRole(f.crUnmatchedName, map[string]string{f.clusterLabelKey: f.clusterLabelValue})).To(Succeed())
	})

	return nil
}

func (f *FineGrainedNamespaceLabelFilter) createTestClusterRole(name string, labels map[string]string) error {
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
	_, err := f.Client.ClientGo.RbacV1().ClusterRoles().Create(f.Ctx, role, metav1.CreateOptions{})
	return err
}

func (f *FineGrainedNamespaceLabelFilter) Backup() error {
	backup := builder.ForBackup(f.VeleroCfg.VeleroNamespace, f.BackupName).
		ResourcePolicies(f.cmName).
		Result()

	By(fmt.Sprintf("Create backup %s directly via the API with no IncludedNamespaces set\n", f.BackupName), func() {
		Expect(f.Client.Kubebuilder.Create(f.Ctx, backup)).To(Succeed(),
			fmt.Sprintf("Failed to create backup %s", f.BackupName))
	})

	By(fmt.Sprintf("Waiting for backup %s to complete\n", f.BackupName), func() {
		Expect(wait.PollUntilContextTimeout(f.Ctx, PollInterval, PollTimeout, true, func(ctx context.Context) (bool, error) {
			got := &velerov1api.Backup{}
			if err := f.Client.Kubebuilder.Get(ctx, types.NamespacedName{Namespace: f.VeleroCfg.VeleroNamespace, Name: f.BackupName}, got); err != nil {
				return false, err
			}
			switch got.Status.Phase {
			case velerov1api.BackupPhaseCompleted:
				return true, nil
			case velerov1api.BackupPhaseFailed, velerov1api.BackupPhasePartiallyFailed,
				velerov1api.BackupPhaseFailedValidation:
				return false, errors.Newf("backup %s ended in phase %s", f.BackupName, got.Status.Phase)
			default:
				return false, nil
			}
		})).To(Succeed(), fmt.Sprintf("Backup %s did not complete successfully", f.BackupName))
	})

	return nil
}

func (f *FineGrainedNamespaceLabelFilter) Destroy() error {
	By("Deleting test cluster roles before restore", func() {
		for _, cr := range f.clusterRoles {
			_ = f.Client.ClientGo.RbacV1().ClusterRoles().Delete(f.Ctx, cr, metav1.DeleteOptions{})
		}
	})
	return f.TestCase.Destroy()
}

func (f *FineGrainedNamespaceLabelFilter) Verify() error {
	// 1. Verify untargeted namespace was never backed up and therefore NOT restored (namespace label selector)
	By(fmt.Sprintf("Verify untargeted namespace %s was NOT restored (excluded by includedNamespacesByLabel)", f.untargetedNS), func() {
		_, err := GetNamespace(f.Ctx, f.Client, f.untargetedNS)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("Untargeted namespace %s should not exist after restore", f.untargetedNS))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Error should be NotFound")
	})

	// 2. Verify targeted namespace WAS restored (namespace label selector)
	By(fmt.Sprintf("Verify targeted namespace %s was restored (included by includedNamespacesByLabel)", f.targetedNS), func() {
		_, err := GetNamespace(f.Ctx, f.Client, f.targetedNS)
		Expect(err).To(Succeed(), fmt.Sprintf("Targeted namespace %s should exist after restore", f.targetedNS))
	})

	// 3. Verify matching ConfigMap was restored (passes global inclusion, fine-grained kind/label/name)
	By("Verify matching ConfigMap app-cfg-included was restored", func() {
		cm, err := GetConfigMap(f.Client.ClientGo, f.targetedNS, "app-cfg-included")
		Expect(err).To(Succeed(), "app-cfg-included should exist in restored namespace")
		Expect(cm.Data["data"]).To(Equal("retained"))
	})

	// 4. Verify debug ConfigMap was excluded by fine-grained name pattern
	By("Verify ConfigMap app-cfg-debug was NOT restored (excluded by fine-grained excludedNames)", func() {
		_, err := GetConfigMap(f.Client.ClientGo, f.targetedNS, "app-cfg-debug")
		Expect(err).To(HaveOccurred(), "app-cfg-debug should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "app-cfg-debug error should be NotFound")
	})

	// 5. Verify ConfigMap with mismatched label was excluded by fine-grained label selector
	By("Verify ConfigMap other-cfg was NOT restored (excluded by fine-grained labelSelector)", func() {
		_, err := GetConfigMap(f.Client.ClientGo, f.targetedNS, "other-cfg")
		Expect(err).To(HaveOccurred(), "other-cfg should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "other-cfg error should be NotFound")
	})

	// 6. Verify Secret was excluded by global resource filter:
	//    Even though namespacedFilterPolicies lists Secret under kinds and matches labels/names,
	//    excludedNamespaceScopedResources: ["secrets"] takes precedence and prevents it from being backed up.
	By("Verify Secret app-secret was NOT restored (globally excluded by excludedNamespaceScopedResources)", func() {
		_, err := GetSecret(f.Client.ClientGo, f.targetedNS, "app-secret")
		Expect(err).To(HaveOccurred(), "app-secret should not exist after restore")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "app-secret error should be NotFound")
	})

	// 7. Verify clusterScopedFilterPolicy for ClusterRoles:
	// a. Matching ClusterRole must be restored
	By(fmt.Sprintf("Verify matching ClusterRole %s was restored", f.crIncluded), func() {
		cr, err := GetClusterRole(f.Ctx, f.Client, f.crIncluded)
		Expect(err).To(Succeed(), fmt.Sprintf("ClusterRole %s should be restored", f.crIncluded))
		Expect(cr.Name).To(Equal(f.crIncluded))
	})

	// b. Debug ClusterRole must NOT be restored (excluded by excludedNames)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by clusterScopedFilterPolicy excludedNames)", f.crDebug), func() {
		_, err := GetClusterRole(f.Ctx, f.Client, f.crDebug)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", f.crDebug))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	// c. ClusterRole with mismatched label must NOT be restored (excluded by labelSelector)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by clusterScopedFilterPolicy labelSelector)", f.crMismatchedLabel), func() {
		_, err := GetClusterRole(f.Ctx, f.Client, f.crMismatchedLabel)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", f.crMismatchedLabel))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	// d. ClusterRole with unmatched name pattern must NOT be restored (excluded by names)
	By(fmt.Sprintf("Verify ClusterRole %s was NOT restored (excluded by clusterScopedFilterPolicy names pattern)", f.crUnmatchedName), func() {
		_, err := GetClusterRole(f.Ctx, f.Client, f.crUnmatchedName)
		Expect(err).To(HaveOccurred(), fmt.Sprintf("ClusterRole %s should not exist after restore", f.crUnmatchedName))
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ClusterRole error should be NotFound")
	})

	return nil
}

func (f *FineGrainedNamespaceLabelFilter) Clean() error {
	if CurrentSpecReport().Failed() && f.VeleroCfg.FailFast {
		fmt.Println("Test case failed and fail fast is enabled. Skip resource clean up.")
		return nil
	}

	for _, cr := range f.clusterRoles {
		_ = f.Client.ClientGo.RbacV1().ClusterRoles().Delete(f.Ctx, cr, metav1.DeleteOptions{})
	}

	if err := DeleteConfigMap(f.Client.ClientGo, f.VeleroCfg.VeleroNamespace, f.cmName); err != nil {
		return errors.Wrap(err, "failed to delete resource policy configmap")
	}

	return f.GetTestCase().Clean()
}
