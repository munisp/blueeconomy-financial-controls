// cvff-worker runs the Temporal worker for the CVFF four-party disbursement
// rail. Every dependency is env-configured and the process fails closed when
// any required value is absent; there is no in-memory fallback.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/munisp/blueeconomy-financial-controls/internal/cvff"
	"github.com/munisp/blueeconomy-financial-controls/internal/fx"
	"github.com/munisp/blueeconomy-financial-controls/internal/ledger"
	"github.com/munisp/blueeconomy-financial-controls/internal/workflow"
	tigerbeetle "github.com/tigerbeetle/tigerbeetle-go"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("cvff-worker: %v", err)
	}
}

func run() error {
	hostPort := required("TEMPORAL_HOST_PORT")
	namespace := required("TEMPORAL_NAMESPACE")
	taskQueue := required("TEMPORAL_TASK_QUEUE")
	databaseURL := required("DATABASE_URL")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	store, err := cvff.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	rates, err := fx.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer rates.Close()

	clusterID, err := tigerbeetle.HexStringToUint128(required("TIGERBEETLE_CLUSTER_ID_HEX"))
	if err != nil {
		return fmt.Errorf("parse TigerBeetle cluster ID: %w", err)
	}
	tbClient, err := tigerbeetle.NewClient(clusterID, splitRequired("TIGERBEETLE_REPLICA_ADDRESSES"))
	if err != nil {
		return fmt.Errorf("create TigerBeetle client: %w", err)
	}
	defer tbClient.Close()

	railConfig, err := railConfigFromEnv()
	if err != nil {
		return err
	}
	ledgerService, err := ledger.New(tbClient, railConfig.NGNLedger, railConfig.FeeCode)
	if err != nil {
		return err
	}
	rail, err := workflow.NewDisbursementRail(store, rates, ledgerService, railConfig)
	if err != nil {
		return err
	}
	activities, err := workflow.NewActivities(store, rail)
	if err != nil {
		return err
	}
	definition, err := workflow.NewCVFFWorkflow(activities)
	if err != nil {
		return err
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: hostPort, Namespace: namespace})
	if err != nil {
		return fmt.Errorf("dial Temporal: %w", err)
	}
	defer temporalClient.Close()

	cvffWorker := worker.New(temporalClient, taskQueue, worker.Options{})
	cvffWorker.RegisterWorkflow(definition.CVFFDisbursementWorkflow)
	registerActivities(cvffWorker, activities)
	log.Printf("cvff-worker: listening on task queue %s (namespace %s)", taskQueue, namespace)
	if err := cvffWorker.Run(worker.InterruptCh()); err != nil {
		return fmt.Errorf("run Temporal worker: %w", err)
	}
	return nil
}

func registerActivities(cvffWorker worker.Worker, activities *workflow.Activities) {
	cvffWorker.RegisterActivityWithOptions(activities.BeginUnderwriting, activityRegisterOptions(workflow.ActivityBeginUnderwriting))
	cvffWorker.RegisterActivityWithOptions(activities.RecordDecision, activityRegisterOptions(workflow.ActivityRecordDecision))
	cvffWorker.RegisterActivityWithOptions(activities.RecordEscalation, activityRegisterOptions(workflow.ActivityRecordEscalation))
	cvffWorker.RegisterActivityWithOptions(activities.Disburse, activityRegisterOptions(workflow.ActivityDisburse))
	cvffWorker.RegisterActivityWithOptions(activities.CommitAudit, activityRegisterOptions(workflow.ActivityCommitAudit))
}

func activityRegisterOptions(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name, DisableAlreadyRegisteredCheck: false}
}

func railConfigFromEnv() (workflow.DisbursementRailConfig, error) {
	parseAccount := func(name string) (tigerbeetle.Uint128, error) {
		id, err := tigerbeetle.HexStringToUint128(required(name))
		if err != nil {
			return tigerbeetle.Uint128{}, fmt.Errorf("parse %s: %w", name, err)
		}
		return id, nil
	}
	var config workflow.DisbursementRailConfig
	var err error
	if config.NGNDebitAccountID, err = parseAccount("CVFF_NGN_DEBIT_ACCOUNT_ID_HEX"); err != nil {
		return config, err
	}
	if config.NGNCreditAccountID, err = parseAccount("CVFF_NGN_CREDIT_ACCOUNT_ID_HEX"); err != nil {
		return config, err
	}
	if config.USDDebitAccountID, err = parseAccount("CVFF_USD_DEBIT_ACCOUNT_ID_HEX"); err != nil {
		return config, err
	}
	if config.USDCreditAccountID, err = parseAccount("CVFF_USD_CREDIT_ACCOUNT_ID_HEX"); err != nil {
		return config, err
	}
	ngnLedger := uint64Env("CVFF_NGN_LEDGER")
	usdLedger := uint64Env("CVFF_USD_LEDGER")
	feeCode := uint64Env("CVFF_FEE_CODE")
	costCode := uint64Env("CVFF_COST_CODE")
	if ngnLedger > uint64(^uint32(0)) || usdLedger > uint64(^uint32(0)) ||
		feeCode > uint64(^uint16(0)) || costCode > uint64(^uint16(0)) {
		return config, errors.New("CVFF ledger or code exceeds protocol width")
	}
	config.NGNLedger = uint32(ngnLedger)
	config.USDLedger = uint32(usdLedger)
	config.FeeCode = uint16(feeCode)
	config.CostCode = uint16(costCode)
	config.FeeBasisPoints = uint64Env("CVFF_CUSTODIAL_FEE_BPS")
	return config, nil
}

func required(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("cvff-worker: %s is required", name)
	}
	return value
}

func splitRequired(name string) []string {
	value := required(name)
	parts := strings.Split(value, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
		if parts[index] == "" {
			log.Fatalf("cvff-worker: %s contains an empty address", name)
		}
	}
	return parts
}

func uint64Env(name string) uint64 {
	value := required(name)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		log.Fatalf("cvff-worker: %s is not an unsigned integer: %v", name, err)
	}
	return parsed
}
