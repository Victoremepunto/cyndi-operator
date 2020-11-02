package controllers

import (
	"context"
	cyndi "cyndi-operator/api/v1beta1"
	connect "cyndi-operator/controllers/connect"
	"cyndi-operator/controllers/database"
	"cyndi-operator/controllers/utils"
	"cyndi-operator/test"
	"fmt"

	. "cyndi-operator/controllers/config"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("Integration tests", func() {
	var (
		namespacedName       types.NamespacedName
		dbParams             DBParams
		hbiDb                database.Database
		appDb                *database.AppDatabase
		cyndiReconciler      *CyndiPipelineReconciler
		validationReconciler *ValidationReconciler
	)

	var reconcile = func(reconcilers ...reconcile.Reconciler) (pipeline *cyndi.CyndiPipeline) {
		for _, r := range reconcilers {
			result, err := r.Reconcile(ctrl.Request{NamespacedName: namespacedName})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		}

		return getPipeline(namespacedName)
	}

	var seedAppTable = func(db database.Database, TestTable string, ids ...string) {
		template := `INSERT INTO %s (id, account, display_name, tags, updated, created, stale_timestamp, system_profile) VALUES ('%s', '000001', 'test01', '{}', NOW(), NOW(), NOW(), '{}')`

		for _, id := range ids {
			_, err := db.Exec(fmt.Sprintf(template, TestTable, id))
			Expect(err).ToNot(HaveOccurred())
		}
	}

	BeforeEach(func() {
		namespacedName = types.NamespacedName{
			Name:      "integration-test-pipeline",
			Namespace: test.UniqueNamespace(),
		}

		createConfigMap(namespacedName.Namespace, "cyndi", map[string]string{
			"init.validation.attempts.threshold":   "5",
			"init.validation.percentage.threshold": "20",
			"validation.attempts.threshold":        "3",
			"validation.percentage.threshold":      "20",
		})

		cyndiReconciler = newCyndiReconciler()
		validationReconciler = NewValidationReconciler(test.Client, test.Clientset, scheme.Scheme, logf.Log.WithName("test"), false)

		dbParams = getDBParams()

		createDbSecret(namespacedName.Namespace, "host-inventory-db", dbParams)
		createDbSecret(namespacedName.Namespace, fmt.Sprintf("%s-db", namespacedName.Name), dbParams)

		appDb = database.NewAppDatabase(&dbParams)
		err := appDb.Connect()
		Expect(err).ToNot(HaveOccurred())

		_, err = appDb.Exec(`DROP SCHEMA IF EXISTS "inventory" CASCADE; CREATE SCHEMA "inventory";`)
		Expect(err).ToNot(HaveOccurred())

		hbiDb = database.NewBaseDatabase(&dbParams)
		err = hbiDb.Connect()
		Expect(err).ToNot(HaveOccurred())

		_, err = hbiDb.Exec(`DROP TABLE IF EXISTS public.hosts; CREATE TABLE public.hosts (id uuid PRIMARY KEY, canonical_facts jsonb);`)
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		appDb.Close()
		hbiDb.Close()
	})

	Describe("Normal procedures", func() {
		It("Creates a new InsightsOnly pipeline", func() {
			var (
				insightsHosts = []string{
					"3b8c0b37-6208-4323-b7df-030fee22db0c",
					"99d28b1e-aad8-4ac0-8d98-ef33e7d3856e",
					"14bcbbb5-8837-4d24-8122-1d44b65680f5",
				}

				otherHosts = []string{
					"45f639ff-f1f5-4469-9a7b-35295fdb75fc",
					"d2b58af8-fd82-4be1-83b1-1d1071b8bc95",
					"5d378adc-11dc-4791-8f24-cb29e21918a4",
					"f049590f-96ca-47fb-b35c-bcc097a767d7",
				}
			)

			// TODO: move to beforeEach?
			seedTable(hbiDb, "public.hosts", true, insightsHosts...)
			seedTable(hbiDb, "public.hosts", false, otherHosts...)

			createPipeline(namespacedName, &cyndi.CyndiPipelineSpec{InsightsOnly: true})

			pipeline := getPipeline(namespacedName)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_NEW))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionUnknown))

			// start initial sync
			pipeline = reconcile(cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionUnknown))

			// keeps validating while initial sync is in progress
			for i := 1; i < 4; i++ {
				pipeline = reconcile(validationReconciler, cyndiReconciler)
				Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
				Expect(pipeline.GetValid()).To(Equal(metav1.ConditionFalse))
				Expect(pipeline.Status.HostCount).To(Equal(int64(0)))
				Expect(pipeline.Status.ValidationFailedCount).To(Equal(int64(i)))
			}

			// keeps validating while the first few hosts are replicated
			appTable := fmt.Sprintf("inventory.%s", pipeline.Status.TableName)
			seedAppTable(appDb, appTable, insightsHosts[0:2]...)

			pipeline = reconcile(validationReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionFalse))
			Expect(pipeline.Status.HostCount).To(Equal(int64(2)))

			// complete the initial sync
			seedAppTable(appDb, appTable, insightsHosts[2:3]...)
			pipeline = reconcile(validationReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_VALID))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionTrue))
			Expect(pipeline.Status.HostCount).To(Equal(int64(3)))

			// transition to valid and create inventory.hosts view
			pipeline = reconcile(cyndiReconciler)
			Expect(pipeline.IsValid()).To(BeTrue())
			activeTable, err := appDb.GetCurrentTable()
			Expect(err).ToNot(HaveOccurred())
			Expect(*activeTable).To(Equal(pipeline.Status.ActiveTableName))

			// further reconcilations are noop
			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.IsValid()).To(BeTrue())
		})

		It("Refreshes a pipeline when it becomes out of sync", func() {
			var (
				insightsHosts = []string{
					"0038cb4d-665b-4e94-87ab-a5b8a50916c5",
					"64d799f2-2645-4818-b61a-daa53e805a72",
					"2af6bf52-e681-477b-ae5f-72e449da32e4",
				}
			)

			seedTable(hbiDb, "public.hosts", true, insightsHosts...)

			createPipeline(namespacedName, &cyndi.CyndiPipelineSpec{InsightsOnly: true})

			pipeline := getPipeline(namespacedName)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_NEW))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionUnknown))

			// start initial sync
			pipeline = reconcile(cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionUnknown))

			appTable1 := pipeline.Status.TableName
			seedAppTable(appDb, fmt.Sprintf("inventory.%s", appTable1), insightsHosts...)

			// transitions to valid as all hosts are replicated
			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_VALID))
			Expect(pipeline.IsValid()).To(BeTrue())
			Expect(pipeline.Status.HostCount).To(Equal(int64(3)))

			// add a new host to HBI which "fails" to get replicated
			newHost := "0ce8b6a5-32f0-4152-995a-73a390d89744"
			seedTable(hbiDb, "public.hosts", true, newHost)

			for i := 1; i < 4; i++ {
				pipeline = reconcile(validationReconciler, cyndiReconciler)
				Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INVALID))
				Expect(pipeline.Status.ValidationFailedCount).To(Equal(int64(i)))
			}

			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_NEW))
			Expect(pipeline.Status.ActiveTableName).To(Equal(appTable1))

			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
			Expect(pipeline.Status.ActiveTableName).To(Equal(appTable1))
			Expect(pipeline.Status.TableName).ToNot(Equal(pipeline.Status.ActiveTableName))

			appTable2 := pipeline.Status.TableName
			seedAppTable(appDb, fmt.Sprintf("inventory.%s", appTable2), insightsHosts...)
			seedAppTable(appDb, fmt.Sprintf("inventory.%s", appTable2), newHost)

			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_VALID))
			Expect(pipeline.IsValid()).To(BeTrue())
			Expect(pipeline.Status.HostCount).To(Equal(int64(4)))
			Expect(appTable2).To(Equal(pipeline.Status.ActiveTableName))
		})

		It("Removes a pipeline", func() {
			var (
				insightsHosts = []string{
					"0038cb4d-665b-4e94-87ab-a5b8a50916c5",
					"64d799f2-2645-4818-b61a-daa53e805a72",
					"2af6bf52-e681-477b-ae5f-72e449da32e4",
				}
			)

			seedTable(hbiDb, "public.hosts", true, insightsHosts...)

			createPipeline(namespacedName, &cyndi.CyndiPipelineSpec{InsightsOnly: true})

			pipeline := reconcile(cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_INITIAL_SYNC))
			Expect(pipeline.GetValid()).To(Equal(metav1.ConditionUnknown))

			appTable := pipeline.Status.TableName
			seedAppTable(appDb, fmt.Sprintf("inventory.%s", appTable), insightsHosts...)

			// transitions to valid as all hosts are replicated
			pipeline = reconcile(validationReconciler, cyndiReconciler)
			Expect(pipeline.GetState()).To(Equal(cyndi.STATE_VALID))

			err := test.Client.Delete(context.TODO(), pipeline)
			Expect(err).ToNot(HaveOccurred())

			result, err := validationReconciler.Reconcile(ctrl.Request{NamespacedName: namespacedName})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(BeZero())

			result, err = cyndiReconciler.Reconcile(ctrl.Request{NamespacedName: namespacedName})
			Expect(err).ToNot(HaveOccurred())
			Expect(result).To(BeZero())

			pipeline, err = utils.FetchCyndiPipeline(test.Client, namespacedName)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			connectors, err := connect.GetConnectorsForApp(test.Client, namespacedName.Namespace, namespacedName.Name)
			Expect(err).ToNot(HaveOccurred())
			Expect(connectors.Items).To(BeEmpty())

			tables, err := appDb.GetCyndiTables()
			Expect(err).ToNot(HaveOccurred())
			Expect(tables).To(BeEmpty())
		})
	})
})