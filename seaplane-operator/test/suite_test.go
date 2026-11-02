package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	seaplanedb "github.com/openshift-online/rosa-seaplane-api/seaplane-db"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	seaplanev1alpha1 "github.com/openshift-online/rosa-seaplane-api/api/v1alpha1"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/controller"
	dynamo "github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/dynamo/statusstream"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/render"
	"github.com/openshift-online/rosa-seaplane-api/seaplane-operator/internal/silence"
	hd "github.com/openshift-online/rosa-seaplane-kube-applier/seaplane-dynamo/dynamodb"
)

const (
	ddbContainerName = "seaplane-test-dynamodb"
	pgContainerName  = "seaplane-test-postgres"
	amContainerName  = "seaplane-test-alertmanager"
	mc               = "mc01"

	postgresImage         = "quay.io/sclorg/postgresql-16-c10s"
	dynamoLocalImage      = "public.ecr.aws/aws-dynamodb-local/aws-dynamodb-local:latest"
	alertmanagerImage     = "quay.io/prometheus/alertmanager:v0.28.1"
	containerStartTimeout = 5 * time.Minute
)

var (
	ctx           context.Context
	cancel        context.CancelFunc
	mgr           manager.Manager
	k8sClient     client.Client
	dynamoDBCli   *dynamodb.Client
	dynamoCli     *dynamo.Client
	ddbPort       string
	pgPort        string
	amPort        string
	amBaseURL     string
	silenceClient *silence.AlertmanagerClient
	eventRouter   *controller.EventRouter
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	SetDefaultEventuallyTimeout(30 * time.Second)
	SetDefaultEventuallyPollingInterval(500 * time.Millisecond)
	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	containerTool := os.Getenv("CONTAINER_TOOL")
	if containerTool == "" {
		containerTool = "podman"
	}
	logger := logf.Log.WithName("statusstream-test")

	// ── Postgres ──

	By("starting Postgres container")
	pgPort = freePort()
	cmd := exec.Command(
		containerTool, "run", "-d", "--rm",
		"--name", pgContainerName,
		"-e", "POSTGRESQL_DATABASE=pgruntime_test",
		"-e", "POSTGRESQL_USER=test",
		"-e", "POSTGRESQL_PASSWORD=test",
		"-p", fmt.Sprintf("%s:5432", pgPort),
		postgresImage,
	)
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "start postgres: %s", string(out))

	dsn := fmt.Sprintf("postgres://test:test@127.0.0.1:%s/pgruntime_test?sslmode=disable", pgPort)

	By("waiting for Postgres to become ready")
	// TCP port-open is insufficient: the sclorg image initializes the database
	// after the port starts listening. Use pg_isready so we wait until Postgres
	// is actually accepting protocol-level connections.
	Eventually(func() error {
		// Bind the command with a deadline to prevent indefinite blocking.
		opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(opCtx, containerTool, "exec", pgContainerName,
			"pg_isready", "-U", "test", "-d", "pgruntime_test").Run()
	}, 30*time.Second, 500*time.Millisecond).Should(Succeed(), "Postgres did not become ready")

	// ── DynamoDB Local ──

	By("starting DynamoDB Local container")
	ddbPort = freePort()
	cmd = exec.Command(
		containerTool, "run", "-d", "--rm",
		"--name", ddbContainerName,
		"-p", fmt.Sprintf("%s:8000", ddbPort),
		dynamoLocalImage,
	)
	out, err = cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "start DynamoDB Local: %s", string(out))

	dynamoDBCli = dynamodb.NewFromConfig(aws.Config{
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", "test"),
		BaseEndpoint: aws.String(fmt.Sprintf("http://127.0.0.1:%s", ddbPort)),
	})

	Eventually(func() error {
		// Bind the ListTables call with a deadline to prevent indefinite blocking.
		opCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := dynamoDBCli.ListTables(opCtx, &dynamodb.ListTablesInput{})
		return err
	}, 30*time.Second, 500*time.Millisecond).Should(Succeed(), "DynamoDB Local did not become ready")

	By("creating DynamoDB tables")
	createTables(dynamoDBCli)
	dynamoCli = dynamo.NewClient(dynamoDBCli)

	// ── Alertmanager ──

	By("pulling Alertmanager image")
	// Nested rootless podman in OpenShift CI can fail unpacking layers with lchown
	// errors; pre-pull with ignore_chown_errors matches other integration containers.
	out, err = runContainerCommand(containerTool, containerStartTimeout,
		"pull", "--storage-opt", "ignore_chown_errors=true", alertmanagerImage,
	)
	Expect(err).NotTo(HaveOccurred(), "pull Alertmanager: %s", string(out))

	By("starting Alertmanager container")
	amPort = freePort()
	out, err = runContainerCommand(containerTool, containerStartTimeout,
		"run", "-d", "--rm",
		"--storage-opt", "ignore_chown_errors=true",
		"--user", "0:0",
		"--tmpfs", "/alertmanager:rw,exec",
		"--name", amContainerName,
		"-p", fmt.Sprintf("%s:9093", amPort),
		"--pull=never",
		alertmanagerImage,
	)
	Expect(err).NotTo(HaveOccurred(), "start Alertmanager: %s", string(out))

	amBaseURL = fmt.Sprintf("http://127.0.0.1:%s", amPort)
	silenceClient = silence.NewAlertmanagerClient(amBaseURL, nil)

	Eventually(func() error {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%s", amPort), time.Second)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed(), "Alertmanager did not become ready")

	// ── pgruntime Manager ──

	By("creating pgruntime manager")
	scheme := runtime.NewScheme()
	Expect(seaplanev1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(hypershiftv1beta1.AddToScheme(scheme)).To(Succeed())

	var mgrErr error
	mgr, mgrErr = seaplanedb.NewManager(seaplanedb.Options{
		Scheme: scheme,
		DSN:    dsn,
		Logger: logf.Log.WithName("pgruntime"),
	})
	Expect(mgrErr).NotTo(HaveOccurred())

	k8sClient = mgr.GetClient()

	By("seeding ManagementCluster CR")
	mcCR := &seaplanev1alpha1.ManagementCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "mc01"},
		Spec: seaplanev1alpha1.ManagementClusterSpec{
			Region:    "us-east-1",
			AccountID: "111222333444",
		},
	}
	Expect(k8sClient.Create(ctx, mcCR)).To(Succeed())

	// ── Controllers ──

	By("wiring controllers")
	eventRouter = controller.NewEventRouter()
	clusterStatusEvents := make(chan event.GenericEvent, 256)
	nodePoolStatusEvents := make(chan event.GenericEvent, 256)
	manifestStatusEvents := make(chan event.GenericEvent, 256)

	Expect((&controller.PlacementReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.DNSReservationReconciler{
		Client:         mgr.GetClient(),
		APIReader:      mgr.GetAPIReader(),
		Scheme:         mgr.GetScheme(),
		RegionalConfig: render.RegionalConfig{BaseDomainSuffix: "e2e.example.com"},
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.ClusterReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Dynamo: dynamoCli,
		RegionalConfig: render.RegionalConfig{
			BaseDomainSuffix: "e2e.example.com",
			AWSRegion:        "us-east-1",
		},
		StatusEvents: clusterStatusEvents,
		EventRouter:  eventRouter,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.NodePoolReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Dynamo:       dynamoCli,
		StatusEvents: nodePoolStatusEvents,
		EventRouter:  eventRouter,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.ManifestReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		Dynamo:       dynamoCli,
		StatusEvents: manifestStatusEvents,
		EventRouter:  eventRouter,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&controller.SilenceReconciler{
		Client:        mgr.GetClient(),
		SilenceClient: silenceClient,
	}).SetupWithManager(mgr)).To(Succeed())

	// ── Start pgruntime Manager ──

	By("starting pgruntime manager")
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()

	// Wait for cache sync before tests run.
	Eventually(func() bool {
		return mgr.GetCache().WaitForCacheSync(ctx)
	}, 10*time.Second, 100*time.Millisecond).Should(BeTrue(), "pgruntime cache did not sync")

	// ── DynamoDB status stream watchers ──

	By("starting DynamoDB status stream watchers")
	streamMgr := statusstream.NewManager(
		dynamoDBCli,
		mgr.GetClient(),
		[]string{dynamo.TableSuffixStatusApplyDesires, dynamo.TableSuffixStatusReadDesires},
		func(documentID string, _ hd.Item) { eventRouter.Dispatch(documentID) },
		logger.WithName("watcher"),
		hd.Options{
			PollInterval:      3 * time.Second,
			RelistInterval:    10 * time.Second,
			MaxLookbackWindow: 10 * time.Second,
		},
	)
	go streamMgr.Run(ctx, 5*time.Second)

	// ── kube-applier-aws simulators ──

	// Simulate kube-applier-aws: poll specs-applydesires and write status
	// entries with Successful=True so controllers see apply confirmations.
	go func() {
		defer GinkgoRecover()
		specsTable := mc + "-specs-applydesires"
		statusTable := mc + "-status-applydesires"

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				out, err := dynamoDBCli.Scan(ctx, &dynamodb.ScanInput{
					TableName: aws.String(specsTable),
				})
				if err != nil {
					continue
				}
				for _, item := range out.Items {
					docID, ok := item["documentID"]
					if !ok {
						continue
					}
					var observedTime time.Time
					if ut, ok := item["updateTime"]; ok {
						if sv, ok := ut.(*dynamodbtypes.AttributeValueMemberS); ok {
							observedTime, _ = time.Parse(time.RFC3339, sv.Value)
						}
					}
					completeStatus := dynamo.ApplyDesireStatus{
						ObservedDesireUpdateTime: observedTime,
						Conditions: []metav1.Condition{{
							Type:               dynamo.DesireConditionSuccessful,
							Status:             metav1.ConditionTrue,
							Reason:             "NoErrors",
							LastTransitionTime: metav1.Now(),
						}},
					}
					statusAttrs, err := attributevalue.MarshalMap(completeStatus)
					if err != nil {
						continue
					}
					statusItem := map[string]dynamodbtypes.AttributeValue{
						"documentID": docID,
						"updateTime": &dynamodbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
						"shard":      &dynamodbtypes.AttributeValueMemberS{Value: hd.ComputeShardDefault(docID.(*dynamodbtypes.AttributeValueMemberS).Value)},
						"status":     &dynamodbtypes.AttributeValueMemberM{Value: statusAttrs},
					}
					_, _ = dynamoDBCli.PutItem(ctx, &dynamodb.PutItemInput{
						TableName: aws.String(statusTable),
						Item:      statusItem,
					})
				}
			}
		}
	}()

	// ── kube-applier-aws readdesires simulator ──

	// Simulate kube-applier-aws: poll specs-readdesires and write status
	// entries with fabricated KubeContent (a completed Job).
	go func() {
		defer GinkgoRecover()
		specsTable := mc + "-specs-readdesires"
		statusTable := mc + "-status-readdesires"
		completedJob := []byte(
			`{"apiVersion":"batch/v1","kind":"Job",` +
				`"metadata":{"name":"e2e-job-abc123","namespace":"e2e-actions"},` +
				`"status":{"succeeded":1,"startTime":"2026-06-25T10:00:00Z",` +
				`"completionTime":"2026-06-25T10:00:05Z"}}`,
		)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				out, err := dynamoDBCli.Scan(ctx, &dynamodb.ScanInput{
					TableName: aws.String(specsTable),
				})
				if err != nil {
					continue
				}
				for _, item := range out.Items {
					docID, ok := item["documentID"]
					if !ok {
						continue
					}
					_, err = dynamoDBCli.PutItem(ctx, &dynamodb.PutItemInput{
						TableName: aws.String(statusTable),
						Item: map[string]dynamodbtypes.AttributeValue{
							"documentID":         docID,
							"updateTime":         &dynamodbtypes.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
							"shard":              &dynamodbtypes.AttributeValueMemberS{Value: hd.ComputeShardDefault(docID.(*dynamodbtypes.AttributeValueMemberS).Value)},
							"status_kubeContent": &dynamodbtypes.AttributeValueMemberS{Value: string(completedJob)},
						},
					})
					Expect(err).NotTo(HaveOccurred(), "write simulated ReadDesire status")
				}
			}
		}
	}()
})

var _ = AfterSuite(func() {
	By("stopping pgruntime manager")
	cancel()

	containerTool := os.Getenv("CONTAINER_TOOL")
	if containerTool == "" {
		containerTool = "podman"
	}

	By("stopping Postgres container")
	removeContainer(containerTool, pgContainerName)

	By("stopping DynamoDB Local container")
	removeContainer(containerTool, ddbContainerName)

	By("stopping Alertmanager container")
	removeContainer(containerTool, amContainerName)
})

func freePort() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return fmt.Sprintf("%d", port)
}

func runContainerCommand(tool string, timeout time.Duration, args ...string) ([]byte, error) {
	startCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return exec.CommandContext(startCtx, tool, args...).CombinedOutput()
}

func removeContainer(tool, name string) {
	if _, err := runContainerCommand(tool, 30*time.Second, "rm", "-f", name); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "failed to remove container %s: %v\n", name, err)
	}
}

func createTables(db *dynamodb.Client) {
	suffixes := []string{"-applydesires", "-readdesires"}
	prefixes := []string{mc + "-specs", mc + "-status"}

	for _, prefix := range prefixes {
		for _, suffix := range suffixes {
			tableName := prefix + suffix
			input := &dynamodb.CreateTableInput{
				TableName: aws.String(tableName),
				AttributeDefinitions: []dynamodbtypes.AttributeDefinition{
					{
						AttributeName: aws.String("documentID"),
						AttributeType: dynamodbtypes.ScalarAttributeTypeS,
					},
					{
						AttributeName: aws.String("shard"),
						AttributeType: dynamodbtypes.ScalarAttributeTypeS,
					},
					{
						AttributeName: aws.String("updateTime"),
						AttributeType: dynamodbtypes.ScalarAttributeTypeS,
					},
				},
				KeySchema: []dynamodbtypes.KeySchemaElement{
					{
						AttributeName: aws.String("documentID"),
						KeyType:       dynamodbtypes.KeyTypeHash,
					},
				},
				GlobalSecondaryIndexes: []dynamodbtypes.GlobalSecondaryIndex{
					{
						IndexName: aws.String("updateTime-index"),
						KeySchema: []dynamodbtypes.KeySchemaElement{
							{AttributeName: aws.String("shard"), KeyType: dynamodbtypes.KeyTypeHash},
							{AttributeName: aws.String("updateTime"), KeyType: dynamodbtypes.KeyTypeRange},
						},
						Projection: &dynamodbtypes.Projection{
							ProjectionType: dynamodbtypes.ProjectionTypeAll,
						},
					},
				},
				BillingMode: dynamodbtypes.BillingModePayPerRequest,
			}
			_, err := db.CreateTable(context.Background(), input)
			Expect(err).NotTo(HaveOccurred(), "create table %s", tableName)
		}
	}
}
