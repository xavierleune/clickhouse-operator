// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chi

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	core "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"

	log "github.com/altinity/clickhouse-operator/pkg/announcer"
	api "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/apis/swversion"
	"github.com/altinity/clickhouse-operator/pkg/chop"
	"github.com/altinity/clickhouse-operator/pkg/controller"
	"github.com/altinity/clickhouse-operator/pkg/controller/chi/kube"
	"github.com/altinity/clickhouse-operator/pkg/controller/chi/metrics"
	"github.com/altinity/clickhouse-operator/pkg/controller/common"
	"github.com/altinity/clickhouse-operator/pkg/controller/common/poller"
	"github.com/altinity/clickhouse-operator/pkg/controller/common/statefulset"
	"github.com/altinity/clickhouse-operator/pkg/controller/common/storage"
	"github.com/altinity/clickhouse-operator/pkg/interfaces"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/config"
	"github.com/altinity/clickhouse-operator/pkg/model/common/action_plan"
	"github.com/altinity/clickhouse-operator/pkg/model/zookeeper"
	"github.com/altinity/clickhouse-operator/pkg/util"
)

// reconcileCHI run reconcile cycle for a CHI
func (w *worker) reconcileCHI(ctx context.Context, old, new *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.logOldAndNew("non-normalized yet (native)", old, new)

	switch {
	case w.isAfterFinalizerInstalled(old, new):
		w.a.M(new).F().Info("isAfterFinalizerInstalled - continue reconcile-1")
	case w.isGenerationTheSame(old, new):
		w.a.M(new).F().Info("isGenerationTheSame() - nothing to do here, exit")
		return nil
	}

	w.a.M(new).S().P()
	defer w.a.M(new).E().P()

	metrics.CHIInitZeroValues(ctx, new)
	metrics.CHIReconcilesStarted(ctx, new)
	startTime := time.Now()

	w.a.M(new).F().Info("Changing OLD to Normalized COMPLETED: %s/%s", new.Namespace, new.Name)

	if new.HasAncestor() {
		w.a.M(new).F().Info("has ancestor, use it as a base for reconcile. CHI: %s/%s", new.Namespace, new.Name)
		old = new.GetAncestor()
	} else {
		w.a.M(new).F().Info("has NO ancestor, use empty CHI as a base for reconcile. CHI: %s/%s", new.Namespace, new.Name)
		old = nil
	}

	w.a.M(new).F().Info("Normalized OLD CHI: %s/%s", new.Namespace, new.Name)
	old = w.normalize(old)

	w.a.M(new).F().Info("Normalized NEW CHI: %s/%s", new.Namespace, new.Name)
	new = w.normalize(new)

	new.SetAncestor(old)
	w.logOldAndNew("normalized", old, new)

	actionPlan := action_plan.NewActionPlan(old, new)
	w.logActionPlan(actionPlan)

	switch {
	case actionPlan.HasActionsToDo():
		w.a.M(new).F().Info("ActionPlan has actions - continue reconcile")
	case w.isAfterFinalizerInstalled(old, new):
		w.a.M(new).F().Info("isAfterFinalizerInstalled - continue reconcile-2")
	default:
		w.a.M(new).F().Info("ActionPlan has no actions and no need to install finalizer - nothing to do")
		return nil
	}

	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.newTask(new)
	w.markReconcileStart(ctx, new, actionPlan)
	w.excludeStoppedCHIFromMonitoring(new)
	w.walkHosts(ctx, new, actionPlan)

	if err := w.reconcile(ctx, new); err != nil {
		// Something went wrong
		w.a.WithEvent(new, common.EventActionReconcile, common.EventReasonReconcileFailed).
			WithStatusError(new).
			M(new).F().
			Error("FAILED to reconcile CHI err: %v", err)
		w.markReconcileCompletedUnsuccessfully(ctx, new, err)
		if errors.Is(err, common.ErrCRUDAbort) {
			metrics.CHIReconcilesAborted(ctx, new)
		}
	} else {
		// Reconcile successful
		// Post-process added items
		if util.IsContextDone(ctx) {
			log.V(2).Info("task is done")
			return nil
		}
		w.clean(ctx, new)
		w.dropReplicas(ctx, new, actionPlan)
		w.addCHIToMonitoring(new)
		w.waitForIPAddresses(ctx, new)
		w.finalizeReconcileAndMarkCompleted(ctx, new)

		metrics.CHIReconcilesCompleted(ctx, new)
		metrics.CHIReconcilesTimings(ctx, new, time.Now().Sub(startTime).Seconds())
	}

	return nil
}

// ReconcileShardsAndHostsOptionsCtxKeyType specifies type for ReconcileShardsAndHostsOptionsCtxKey
// More details here on why do we need special type
// https://stackoverflow.com/questions/40891345/fix-should-not-use-basic-type-string-as-key-in-context-withvalue-golint
type ReconcileShardsAndHostsOptionsCtxKeyType string

// ReconcileShardsAndHostsOptionsCtxKey specifies name of the key to be used for ReconcileShardsAndHostsOptions
const ReconcileShardsAndHostsOptionsCtxKey ReconcileShardsAndHostsOptionsCtxKeyType = "ReconcileShardsAndHostsOptions"

// reconcile reconciles ClickHouseInstallation
func (w *worker) reconcile(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().P()
	defer w.a.V(2).M(chi).E().P()

	counters := api.NewHostReconcileAttributesCounters()
	chi.WalkHosts(func(host *api.Host) error {
		counters.Add(host.GetReconcileAttributes())
		return nil
	})

	if counters.AddOnly() {
		w.a.V(1).M(chi).Info("Enabling full fan-out mode. CHI: %s", util.NamespaceNameString(chi))
		ctx = context.WithValue(ctx, ReconcileShardsAndHostsOptionsCtxKey, &ReconcileShardsAndHostsOptions{
			fullFanOut: true,
		})
	}

	return chi.WalkTillError(
		ctx,
		w.reconcileCHIAuxObjectsPreliminary,
		w.reconcileCluster,
		w.reconcileShardsAndHosts,
		w.reconcileCHIAuxObjectsFinal,
	)
}

// reconcileCHIAuxObjectsPreliminary reconciles CHI preliminary in order to ensure that ConfigMaps are in place
func (w *worker) reconcileCHIAuxObjectsPreliminary(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().P()
	defer w.a.V(2).M(chi).E().P()

	// CHI common ConfigMap without added hosts
	chi.GetRuntime().LockCommonConfig()
	if err := w.reconcileCHIConfigMapCommon(ctx, chi, w.options()); err != nil {
		w.a.F().Error("failed to reconcile config map common. err: %v", err)
	}
	chi.GetRuntime().UnlockCommonConfig()

	// 3. CHI users ConfigMap
	if err := w.reconcileCHIConfigMapUsers(ctx, chi); err != nil {
		w.a.F().Error("failed to reconcile config map users. err: %v", err)
	}

	return nil
}

// reconcileCHIServicePreliminary runs first stage of CHI reconcile process
func (w *worker) reconcileCHIServicePreliminary(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if chi.IsStopped() {
		// Stopped CHI must have no entry point
		_ = w.c.deleteServiceCHI(ctx, chi)
	}
	return nil
}

// reconcileCHIServiceFinal runs second stage of CHI reconcile process
func (w *worker) reconcileCHIServiceFinal(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if chi.IsStopped() {
		// Stopped CHI must have no entry point
		return nil
	}

	// Create entry point for the whole CHI
	if service := w.task.Creator().CreateService(interfaces.ServiceCR); service != nil {
		if err := w.reconcileService(ctx, chi, service); err != nil {
			// Service not reconciled
			w.task.RegistryFailed().RegisterService(service.GetObjectMeta())
			return err
		}
		w.task.RegistryReconciled().RegisterService(service.GetObjectMeta())
	}

	return nil
}

// reconcileCHIAuxObjectsFinal reconciles CHI global objects
func (w *worker) reconcileCHIAuxObjectsFinal(ctx context.Context, chi *api.ClickHouseInstallation) (err error) {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().P()
	defer w.a.V(2).M(chi).E().P()

	// CHI ConfigMaps with update
	chi.GetRuntime().LockCommonConfig()
	err = w.reconcileCHIConfigMapCommon(ctx, chi, nil)
	chi.GetRuntime().UnlockCommonConfig()
	return err
}

// reconcileCHIConfigMapCommon reconciles all CHI's common ConfigMap
func (w *worker) reconcileCHIConfigMapCommon(
	ctx context.Context,
	chi *api.ClickHouseInstallation,
	options *config.FilesGeneratorOptionsClickHouse,
) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	// ConfigMap common for all resources in CHI
	// contains several sections, mapped as separated chopConfig files,
	// such as remote servers, zookeeper setup, etc
	configMapCommon := w.task.Creator().CreateConfigMap(interfaces.ConfigMapCHICommon, options)
	err := w.reconcileConfigMap(ctx, chi, configMapCommon)
	if err == nil {
		w.task.RegistryReconciled().RegisterConfigMap(configMapCommon.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterConfigMap(configMapCommon.GetObjectMeta())
	}
	return err
}

// reconcileCHIConfigMapUsers reconciles all CHI's users ConfigMap
// ConfigMap common for all users resources in CHI
func (w *worker) reconcileCHIConfigMapUsers(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	// ConfigMap common for all users resources in CHI
	configMapUsers := w.task.Creator().CreateConfigMap(interfaces.ConfigMapCHICommonUsers)
	err := w.reconcileConfigMap(ctx, chi, configMapUsers)
	if err == nil {
		w.task.RegistryReconciled().RegisterConfigMap(configMapUsers.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterConfigMap(configMapUsers.GetObjectMeta())
	}
	return err
}

// reconcileHostConfigMap reconciles host's personal ConfigMap
func (w *worker) reconcileHostConfigMap(ctx context.Context, host *api.Host) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	// ConfigMap for a host
	configMap := w.task.Creator().CreateConfigMap(interfaces.ConfigMapCHIHost, host)
	err := w.reconcileConfigMap(ctx, host.GetCR(), configMap)
	if err == nil {
		w.task.RegistryReconciled().RegisterConfigMap(configMap.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterConfigMap(configMap.GetObjectMeta())
		return err
	}

	return nil
}

const unknownVersion = "failed to query"

type versionOptions struct {
	skipNew             bool
	skipStopped         bool
	skipStoppedAncestor bool
}

func (opts versionOptions) shouldSkip(host *api.Host) (bool, string) {
	if opts.skipNew && (host.IsNewOne()) {
		return true, "host is a new one, version is not not applicable"
	}

	if opts.skipStopped && host.IsStopped() {
		return true, "host is stopped, version is not applicable"
	}

	if opts.skipStoppedAncestor && host.GetAncestor().IsStopped() {
		return true, "host ancestor is stopped, version is not applicable"
	}

	return false, ""
}

// getHostClickHouseVersion gets host ClickHouse version
func (w *worker) getHostClickHouseVersion(ctx context.Context, host *api.Host, opts versionOptions) (string, error) {
	if skip, description := opts.shouldSkip(host); skip {
		return description, nil
	}

	version, err := w.ensureClusterSchemer(host).HostClickHouseVersion(ctx, host)
	if err != nil {
		w.a.V(1).M(host).F().Warning("Failed to get ClickHouse version on host: %s", host.GetName())
		return unknownVersion, err
	}

	w.a.V(1).M(host).F().Info("Get ClickHouse version on host: %s version: %s", host.GetName(), version)
	host.Runtime.Version = swversion.NewSoftWareVersion(version)

	return version, nil
}

func (w *worker) pollHostForClickHouseVersion(ctx context.Context, host *api.Host) (version string, err error) {
	err = poller.PollHost(
		ctx,
		host,
		nil,
		func(_ctx context.Context, _host *api.Host) bool {
			var e error
			version, e = w.getHostClickHouseVersion(_ctx, _host, versionOptions{skipStopped: true})
			if e == nil {
				return true
			}
			w.a.V(1).M(host).F().Warning("Host is NOT alive: %s ", host.GetName())
			return false
		},
	)
	return
}

// reconcileHostStatefulSet reconciles host's StatefulSet
func (w *worker) reconcileHostStatefulSet(ctx context.Context, host *api.Host, opts ...*statefulset.ReconcileStatefulSetOptions) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	log.V(1).M(host).F().S().Info("reconcile StatefulSet start")
	defer log.V(1).M(host).F().E().Info("reconcile StatefulSet end")

	version, _ := w.getHostClickHouseVersion(ctx, host, versionOptions{skipNew: true, skipStoppedAncestor: true})
	host.Runtime.CurStatefulSet, _ = w.c.kube.STS().Get(host)

	w.a.V(1).M(host).F().Info("Reconcile host: %s. ClickHouse version: %s", host.GetName(), version)
	// In case we have to force-restart host
	// We'll do it via replicas: 0 in StatefulSet.
	if w.shouldForceRestartHost(host) {
		w.a.V(1).M(host).F().Info("Reconcile host: %s. Shutting host down due to force restart", host.GetName())
		w.stsReconciler.PrepareHostStatefulSetWithStatus(ctx, host, true)
		_ = w.stsReconciler.ReconcileStatefulSet(ctx, host, false)
		metrics.HostReconcilesRestart(ctx, host.GetCR())
		// At this moment StatefulSet has 0 replicas.
		// First stage of RollingUpdate completed.
	}

	// We are in place, where we can  reconcile StatefulSet to desired configuration.
	w.a.V(1).M(host).F().Info("Reconcile host: %s. Reconcile StatefulSet", host.GetName())
	w.stsReconciler.PrepareHostStatefulSetWithStatus(ctx, host, false)
	err := w.stsReconciler.ReconcileStatefulSet(ctx, host, true, opts...)
	if err == nil {
		w.task.RegistryReconciled().RegisterStatefulSet(host.Runtime.DesiredStatefulSet.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterStatefulSet(host.Runtime.DesiredStatefulSet.GetObjectMeta())
		if err == common.ErrCRUDIgnore {
			// Pretend nothing happened in case of ignore
			err = nil
		}

		host.GetCR().EnsureStatus().HostFailed()
		w.a.WithEvent(host.GetCR(), common.EventActionReconcile, common.EventReasonReconcileFailed).
			WithStatusAction(host.GetCR()).
			WithStatusError(host.GetCR()).
			M(host).F().
			Error("FAILED to reconcile StatefulSet for host: %s", host.GetName())
	}

	return err
}

// reconcileHostService reconciles host's Service
func (w *worker) reconcileHostService(ctx context.Context, host *api.Host) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}
	service := w.task.Creator().CreateService(interfaces.ServiceCHIHost, host)
	if service == nil {
		// This is not a problem, service may be omitted
		return nil
	}
	err := w.reconcileService(ctx, host.GetCR(), service)
	if err == nil {
		w.a.V(1).M(host).F().Info("DONE Reconcile service of the host: %s", host.GetName())
		w.task.RegistryReconciled().RegisterService(service.GetObjectMeta())
	} else {
		w.a.V(1).M(host).F().Warning("FAILED Reconcile service of the host: %s", host.GetName())
		w.task.RegistryFailed().RegisterService(service.GetObjectMeta())
	}
	return err
}

// reconcileCluster reconciles ChkCluster, excluding nested shards
func (w *worker) reconcileCluster(ctx context.Context, cluster *api.ChiCluster) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(cluster).S().P()
	defer w.a.V(2).M(cluster).E().P()

	// Add ChkCluster's Service
	if service := w.task.Creator().CreateService(interfaces.ServiceCHICluster, cluster); service != nil {
		if err := w.reconcileService(ctx, cluster.Runtime.CHI, service); err == nil {
			w.task.RegistryReconciled().RegisterService(service.GetObjectMeta())
		} else {
			w.task.RegistryFailed().RegisterService(service.GetObjectMeta())
		}
	}

	// Add cluster's Auto Secret
	if cluster.Secret.Source() == api.ClusterSecretSourceAuto {
		if secret := w.task.Creator().CreateClusterSecret(w.c.namer.Name(interfaces.NameClusterAutoSecret, cluster)); secret != nil {
			if err := w.reconcileSecret(ctx, cluster.Runtime.CHI, secret); err == nil {
				w.task.RegistryReconciled().RegisterSecret(secret.GetObjectMeta())
			} else {
				w.task.RegistryFailed().RegisterSecret(secret.GetObjectMeta())
			}
		}
	}

	pdb := w.task.Creator().CreatePodDisruptionBudget(cluster)
	if err := w.reconcilePDB(ctx, cluster, pdb); err == nil {
		w.task.RegistryReconciled().RegisterPDB(pdb.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterPDB(pdb.GetObjectMeta())
	}

	reconcileZookeeperRootPath(cluster)
	return nil
}

func reconcileZookeeperRootPath(cluster *api.ChiCluster) {
	if cluster.Zookeeper.IsEmpty() {
		// Nothing to reconcile
		return
	}
	conn := zookeeper.NewConnection(cluster.Zookeeper.Nodes)
	path := zookeeper.NewPathManager(conn)
	path.Ensure(cluster.Zookeeper.Root)
	path.Close()
}

// getReconcileShardsWorkersNum calculates how many workers are allowed to be used for concurrent shard reconcile
func (w *worker) getReconcileShardsWorkersNum(shards []*api.ChiShard, opts *ReconcileShardsAndHostsOptions) int {
	availableWorkers := float64(chop.Config().Reconcile.Runtime.ReconcileShardsThreadsNumber)
	maxConcurrencyPercent := float64(chop.Config().Reconcile.Runtime.ReconcileShardsMaxConcurrencyPercent)
	_100Percent := float64(100)
	shardsNum := float64(len(shards))

	if opts.FullFanOut() {
		// For full fan-out scenarios use all available workers.
		// Always allow at least 1 worker.
		return int(math.Max(availableWorkers, 1))
	}

	// For non-full fan-out scenarios respect .Reconcile.Runtime.ReconcileShardsMaxConcurrencyPercent.
	// Always allow at least 1 worker.
	maxAllowedWorkers := math.Max(math.Round((maxConcurrencyPercent/_100Percent)*shardsNum), 1)
	return int(math.Min(availableWorkers, maxAllowedWorkers))
}

// ReconcileShardsAndHostsOptions is and options for reconciler
type ReconcileShardsAndHostsOptions struct {
	fullFanOut bool
}

// FullFanOut gets value
func (o *ReconcileShardsAndHostsOptions) FullFanOut() bool {
	if o == nil {
		return false
	}
	return o.fullFanOut
}

// reconcileShardsAndHosts reconciles shards and hosts of each shard
func (w *worker) reconcileShardsAndHosts(ctx context.Context, shards []*api.ChiShard) error {
	// Sanity check - CHI has to have shard(s)
	if len(shards) == 0 {
		return nil
	}

	// Try to fetch options
	opts, ok := ctx.Value(ReconcileShardsAndHostsOptionsCtxKey).(*ReconcileShardsAndHostsOptions)
	if ok {
		w.a.V(1).Info("found ReconcileShardsAndHostsOptionsCtxKey")
	} else {
		w.a.V(1).Info("not found ReconcileShardsAndHostsOptionsCtxKey, use empty opts")
		opts = &ReconcileShardsAndHostsOptions{}
	}

	// Which shard to start concurrent processing with
	var startShard int
	if opts.FullFanOut() {
		// For full fan-out scenarios we'll start shards processing from the very beginning
		startShard = 0
		w.a.V(1).Info("full fan-out requested")
	} else {
		// For non-full fan-out scenarios, we'll process the first shard separately.
		// This gives us some early indicator on whether the reconciliation would fail,
		// and for large clusters it is a small price to pay before performing concurrent fan-out.
		w.a.V(1).Info("starting first shard separately")
		if err := w.reconcileShardWithHosts(ctx, shards[0]); err != nil {
			w.a.V(1).Warning("first shard failed, skipping rest of shards due to an error: %v", err)
			return err
		}

		// Since shard with 0 index is already done, we'll proceed with the 1-st
		startShard = 1
	}

	// Process shards using specified concurrency level while maintaining specified max concurrency percentage.
	// Loop over shards.
	workersNum := w.getReconcileShardsWorkersNum(shards, opts)
	w.a.V(1).Info("Starting rest of shards on workers: %d", workersNum)
	for startShardIndex := startShard; startShardIndex < len(shards); startShardIndex += workersNum {
		endShardIndex := startShardIndex + workersNum
		if endShardIndex > len(shards) {
			endShardIndex = len(shards)
		}
		concurrentlyProcessedShards := shards[startShardIndex:endShardIndex]

		// Processing error protected with mutex
		var err error
		var errLock sync.Mutex

		wg := sync.WaitGroup{}
		wg.Add(len(concurrentlyProcessedShards))
		// Launch shard concurrent processing
		for j := range concurrentlyProcessedShards {
			shard := concurrentlyProcessedShards[j]
			go func() {
				defer wg.Done()
				if e := w.reconcileShardWithHosts(ctx, shard); e != nil {
					errLock.Lock()
					err = e
					errLock.Unlock()
					return
				}
			}()
		}
		wg.Wait()
		if err != nil {
			w.a.V(1).Warning("Skipping rest of shards due to an error: %v", err)
			return err
		}
	}
	return nil
}

func (w *worker) reconcileShardWithHosts(ctx context.Context, shard *api.ChiShard) error {
	if err := w.reconcileShard(ctx, shard); err != nil {
		return err
	}
	for replicaIndex := range shard.Hosts {
		host := shard.Hosts[replicaIndex]
		if err := w.reconcileHost(ctx, host); err != nil {
			return err
		}
	}
	return nil
}

// reconcileShard reconciles specified shard, excluding nested replicas
func (w *worker) reconcileShard(ctx context.Context, shard *api.ChiShard) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(shard).S().P()
	defer w.a.V(2).M(shard).E().P()

	// Add Shard's Service
	service := w.task.Creator().CreateService(interfaces.ServiceCHIShard, shard)
	if service == nil {
		// This is not a problem, ServiceShard may be omitted
		return nil
	}
	err := w.reconcileService(ctx, shard.Runtime.CHI, service)
	if err == nil {
		w.task.RegistryReconciled().RegisterService(service.GetObjectMeta())
	} else {
		w.task.RegistryFailed().RegisterService(service.GetObjectMeta())
	}
	return err
}

// reconcileHost reconciles specified ClickHouse host
func (w *worker) reconcileHost(ctx context.Context, host *api.Host) error {
	var (
		reconcileStatefulSetOpts *statefulset.ReconcileStatefulSetOptions
		migrateTableOpts         *migrateTableOptions
	)

	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(host).S().P()
	defer w.a.V(2).M(host).E().P()

	metrics.HostReconcilesStarted(ctx, host.GetCR())
	startTime := time.Now()

	if host.IsFirst() {
		w.reconcileCHIServicePreliminary(ctx, host.GetCR())
		defer w.reconcileCHIServiceFinal(ctx, host.GetCR())
	}

	// Check whether ClickHouse is running and accessible and what version is available
	if version, err := w.getHostClickHouseVersion(ctx, host, versionOptions{skipNew: true, skipStoppedAncestor: true}); err == nil {
		w.a.V(1).
			WithEvent(host.GetCR(), common.EventActionReconcile, common.EventReasonReconcileStarted).
			WithStatusAction(host.GetCR()).
			M(host).F().
			Info("Reconcile Host start. Host: %s ClickHouse version running: %s", host.GetName(), version)
	} else {
		w.a.V(1).
			WithEvent(host.GetCR(), common.EventActionReconcile, common.EventReasonReconcileStarted).
			WithStatusAction(host.GetCR()).
			M(host).F().
			Warning("Reconcile Host start. Host: %s Failed to get ClickHouse version: %s", host.GetName(), version)
	}

	// Create artifacts
	w.stsReconciler.PrepareHostStatefulSetWithStatus(ctx, host, false)

	if w.excludeHost(ctx, host) {
		// Need to wait to complete queries only in case host is excluded from the cluster
		// In case host is not excluded from the cluster queries would continue to be started on the host
		// and there is no reason to wait for queries to complete. We may wait endlessly.
		_ = w.completeQueries(ctx, host)
	}

	if err := w.reconcileHostConfigMap(ctx, host); err != nil {
		metrics.HostReconcilesErrors(ctx, host.GetCR())
		w.a.V(1).
			M(host).F().
			Warning("Reconcile Host interrupted with an error 2. Host: %s Err: %v", host.GetName(), err)
		return err
	}

	w.setHasData(host)

	w.a.V(1).
		M(host).F().
		Info("Reconcile PVCs and check possible data loss for host: %s", host.GetName())
	if storage.ErrIsDataLoss(
		storage.NewStorageReconciler(
			w.task, w.c.namer, storage.NewStoragePVC(kube.NewPVCClickHouse(w.c.kubeClient)),
		).ReconcilePVCs(ctx, host, api.DesiredStatefulSet),
	) {
		// In case of data loss detection on existing volumes, we need to:
		// 1. recreate StatefulSet
		// 2. run tables migration again
		reconcileStatefulSetOpts = statefulset.NewReconcileStatefulSetOptions(true)
		migrateTableOpts = &migrateTableOptions{
			forceMigrate: true,
			dropReplica:  true,
		}
		w.a.V(1).
			M(host).F().
			Info("Data loss detected for host: %s. Will do force migrate", host.GetName())
	}

	if err := w.reconcileHostStatefulSet(ctx, host, reconcileStatefulSetOpts); err != nil {
		metrics.HostReconcilesErrors(ctx, host.GetCR())
		w.a.V(1).
			M(host).F().
			Warning("Reconcile Host interrupted with an error 3. Host: %s Err: %v", host.GetName(), err)
		return err
	}
	// Polish all new volumes that operator has to create
	_ = storage.NewStorageReconciler(w.task, w.c.namer, storage.NewStoragePVC(kube.NewPVCClickHouse(w.c.kubeClient))).ReconcilePVCs(ctx, host, api.DesiredStatefulSet)

	_ = w.reconcileHostService(ctx, host)

	host.GetReconcileAttributes().UnsetAdd()

	// Prepare for tables migration.
	// Sometimes service needs some time to start after creation|modification before being accessible for usage
	// Check whether ClickHouse is running and accessible and what version is available.
	if version, err := w.pollHostForClickHouseVersion(ctx, host); err == nil {
		w.a.V(1).
			M(host).F().
			Info("Check host for ClickHouse availability before migrating tables. Host: %s ClickHouse version running: %s", host.GetName(), version)
	} else {
		w.a.V(1).
			M(host).F().
			Warning("Check host for ClickHouse availability before migrating tables. Host: %s Failed to get ClickHouse version: %s", host.GetName(), version)
	}
	_ = w.migrateTables(ctx, host, migrateTableOpts)

	if err := w.includeHost(ctx, host); err != nil {
		metrics.HostReconcilesErrors(ctx, host.GetCR())
		w.a.V(1).
			M(host).F().
			Warning("Reconcile Host interrupted with an error 4. Host: %s Err: %v", host.GetName(), err)
		return err
	}

	// Ensure host is running and accessible and what version is available.
	// Sometimes service needs some time to start after creation|modification before being accessible for usage
	if version, err := w.pollHostForClickHouseVersion(ctx, host); err == nil {
		w.a.V(1).
			WithEvent(host.GetCR(), common.EventActionReconcile, common.EventReasonReconcileCompleted).
			WithStatusAction(host.GetCR()).
			M(host).F().
			Info("Reconcile Host completed. Host: %s ClickHouse version running: %s", host.GetName(), version)
	} else {
		w.a.V(1).
			WithEvent(host.GetCR(), common.EventActionReconcile, common.EventReasonReconcileCompleted).
			WithStatusAction(host.GetCR()).
			M(host).F().
			Warning("Reconcile Host completed. Host: %s Failed to get ClickHouse version: %s", host.GetName(), version)
	}

	now := time.Now()
	hostsCompleted := 0
	hostsCount := 0
	host.GetCR().EnsureStatus().HostCompleted()
	if host.GetCR() != nil && host.GetCR().Status != nil {
		hostsCompleted = host.GetCR().Status.GetHostsCompletedCount()
		hostsCount = host.GetCR().Status.GetHostsCount()
	}
	w.a.V(1).
		WithEvent(host.GetCR(), common.EventActionProgress, common.EventReasonProgressHostsCompleted).
		WithStatusAction(host.GetCR()).
		M(host).F().
		Info("[now: %s] %s: %d of %d", now, common.EventReasonProgressHostsCompleted, hostsCompleted, hostsCount)

	_ = w.c.updateCHIObjectStatus(ctx, host.GetCR(), interfaces.UpdateStatusOptions{
		CopyStatusOptions: api.CopyStatusOptions{
			MainFields: true,
		},
	})

	metrics.HostReconcilesCompleted(ctx, host.GetCR())
	metrics.HostReconcilesTimings(ctx, host.GetCR(), time.Now().Sub(startTime).Seconds())

	return nil
}

// reconcilePDB reconciles PodDisruptionBudget
func (w *worker) reconcilePDB(ctx context.Context, cluster *api.ChiCluster, pdb *policy.PodDisruptionBudget) error {
	cur, err := w.c.kubeClient.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Get(ctx, pdb.Name, controller.NewGetOptions())
	switch {
	case err == nil:
		pdb.ResourceVersion = cur.ResourceVersion
		_, err := w.c.kubeClient.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Update(ctx, pdb, controller.NewUpdateOptions())
		if err == nil {
			log.V(1).Info("PDB updated: %s/%s", pdb.Namespace, pdb.Name)
		} else {
			log.Error("FAILED to update PDB: %s/%s err: %v", pdb.Namespace, pdb.Name, err)
			return nil
		}
	case apiErrors.IsNotFound(err):
		_, err := w.c.kubeClient.PolicyV1().PodDisruptionBudgets(pdb.Namespace).Create(ctx, pdb, controller.NewCreateOptions())
		if err == nil {
			log.V(1).Info("PDB created: %s/%s", pdb.Namespace, pdb.Name)
		} else {
			log.Error("FAILED create PDB: %s/%s err: %v", pdb.Namespace, pdb.Name, err)
			return err
		}
	default:
		log.Error("FAILED get PDB: %s/%s err: %v", pdb.Namespace, pdb.Name, err)
		return err
	}

	return nil
}

// reconcileConfigMap reconciles core.ConfigMap which belongs to specified CHI
func (w *worker) reconcileConfigMap(
	ctx context.Context,
	chi *api.ClickHouseInstallation,
	configMap *core.ConfigMap,
) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().P()
	defer w.a.V(2).M(chi).E().P()

	// Check whether this object already exists in k8s
	curConfigMap, err := w.c.getConfigMap(configMap.GetObjectMeta(), true)

	if curConfigMap != nil {
		// We have ConfigMap - try to update it
		err = w.updateConfigMap(ctx, chi, configMap)
	}

	if apiErrors.IsNotFound(err) {
		// ConfigMap not found - even during Update process - try to create it
		err = w.createConfigMap(ctx, chi, configMap)
	}

	if err != nil {
		w.a.WithEvent(chi, common.EventActionReconcile, common.EventReasonReconcileFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			M(chi).F().
			Error("FAILED to reconcile ConfigMap: %s CHI: %s ", configMap.Name, chi.Name)
	}

	return err
}

// hasService checks whether specified service exists
func (w *worker) hasService(ctx context.Context, chi *api.ClickHouseInstallation, service *core.Service) bool {
	// Check whether this object already exists
	curService, _ := w.c.kube.Service().Get(service)
	return curService != nil
}

// reconcileService reconciles core.Service
func (w *worker) reconcileService(ctx context.Context, chi *api.ClickHouseInstallation, service *core.Service) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().Info(service.Name)
	defer w.a.V(2).M(chi).E().Info(service.Name)

	// Check whether this object already exists
	curService, err := w.c.kube.Service().Get(service)

	if curService != nil {
		// We have the Service - try to update it
		w.a.V(1).M(chi).F().Info("Service found: %s/%s. Will try to update", service.Namespace, service.Name)
		err = w.updateService(ctx, chi, curService, service)
	}

	if err != nil {
		if apiErrors.IsNotFound(err) {
			// The Service is either not found or not updated. Try to recreate it
			w.a.V(1).M(chi).F().Info("Service: %s/%s not found. err: %v", service.Namespace, service.Name, err)
		} else {
			// The Service is either not found or not updated. Try to recreate it
			w.a.WithEvent(chi, common.EventActionUpdate, common.EventReasonUpdateFailed).
				WithStatusAction(chi).
				WithStatusError(chi).
				M(chi).F().
				Error("Update Service: %s/%s failed with error: %v", service.Namespace, service.Name, err)
		}

		_ = w.c.deleteServiceIfExists(ctx, service.Namespace, service.Name)
		err = w.createService(ctx, chi, service)
	}

	if err == nil {
		w.a.V(1).M(chi).F().Info("Service reconcile successful: %s/%s", service.Namespace, service.Name)
	} else {
		w.a.WithEvent(chi, common.EventActionReconcile, common.EventReasonReconcileFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			M(chi).F().
			Error("FAILED to reconcile Service: %s/%s CHI: %s ", service.Namespace, service.Name, chi.Name)
	}

	return err
}

// reconcileSecret reconciles core.Secret
func (w *worker) reconcileSecret(ctx context.Context, chi *api.ClickHouseInstallation, secret *core.Secret) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	w.a.V(2).M(chi).S().Info(secret.Name)
	defer w.a.V(2).M(chi).E().Info(secret.Name)

	// Check whether this object already exists
	if _, err := w.c.getSecret(secret); err == nil {
		// We have Secret - try to update it
		return nil
	}

	// Secret not found or broken. Try to recreate
	_ = w.c.deleteSecretIfExists(ctx, secret.Namespace, secret.Name)
	err := w.createSecret(ctx, chi, secret)
	if err != nil {
		w.a.WithEvent(chi, common.EventActionReconcile, common.EventReasonReconcileFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			M(chi).F().
			Error("FAILED to reconcile Secret: %s CHI: %s ", secret.Name, chi.Name)
	}

	return err
}
