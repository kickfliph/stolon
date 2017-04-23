// Copyright 2015 Sorint.lab
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sorintlab/stolon/common"
	"github.com/sorintlab/stolon/pkg/cluster"
	"github.com/sorintlab/stolon/pkg/flagutil"
	"github.com/sorintlab/stolon/pkg/store"
	"github.com/sorintlab/stolon/pkg/timer"
	"github.com/sorintlab/stolon/pkg/util"

	"github.com/davecgh/go-spew/spew"
	"github.com/docker/leadership"
	"github.com/mitchellh/copystructure"
	"github.com/spf13/cobra"
	"github.com/uber-go/zap"
	"golang.org/x/net/context"
)

const (
	fakeStandbyName = "stolonfakestandby"
)

var cmdSentinel = &cobra.Command{
	Use: "stolon-sentinel",
	Run: sentinel,
}

type config struct {
	storeBackend           string
	storeEndpoints         string
	storeCertFile          string
	storeKeyFile           string
	storeCAFile            string
	storeSkipTlsVerify     bool
	clusterName            string
	initialClusterSpecFile string
	debug                  bool
}

var cfg config

func init() {
	cmdSentinel.PersistentFlags().StringVar(&cfg.storeBackend, "store-backend", "", "store backend type (etcd or consul)")
	cmdSentinel.PersistentFlags().StringVar(&cfg.storeEndpoints, "store-endpoints", "", "a comma-delimited list of store endpoints (use https scheme for tls communication) (defaults: http://127.0.0.1:2379 for etcd, http://127.0.0.1:8500 for consul)")
	cmdSentinel.PersistentFlags().StringVar(&cfg.storeCertFile, "store-cert-file", "", "certificate file for client identification to the store")
	cmdSentinel.PersistentFlags().StringVar(&cfg.storeKeyFile, "store-key", "", "private key file for client identification to the store")
	cmdSentinel.PersistentFlags().BoolVar(&cfg.storeSkipTlsVerify, "store-skip-tls-verify", false, "skip store certificate verification (insecure!!!)")
	cmdSentinel.PersistentFlags().StringVar(&cfg.storeCAFile, "store-ca-file", "", "verify certificates of HTTPS-enabled store servers using this CA bundle")
	cmdSentinel.PersistentFlags().StringVar(&cfg.clusterName, "cluster-name", "", "cluster name")
	cmdSentinel.PersistentFlags().StringVar(&cfg.initialClusterSpecFile, "initial-cluster-spec", "", "a file providing the initial cluster specification, used only at cluster initialization, ignored if cluster is already initialized")
	cmdSentinel.PersistentFlags().BoolVar(&cfg.debug, "debug", false, "enable debug logging")
}

var log = zap.New(zap.NewTextEncoder(), zap.AddCaller())

func stderr(format string, a ...interface{}) {
	out := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, strings.TrimSuffix(out, "\n"))
}

func stdout(format string, a ...interface{}) {
	out := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stdout, strings.TrimSuffix(out, "\n"))
}

func die(format string, a ...interface{}) {
	stderr(format, a...)
	os.Exit(1)
}

func (s *Sentinel) electionLoop() {
	for {
		log.Info("Trying to acquire sentinels leadership")
		electedCh, errCh := s.candidate.RunForElection()
		for {
			select {
			case elected := <-electedCh:
				s.leaderMutex.Lock()
				if elected {
					log.Info("sentinel leadership acquired")
					s.leader = true
					s.leadershipCount++
				} else {
					if s.leader {
						log.Info("sentinel leadership lost")
					}
					s.leader = false
				}
				s.leaderMutex.Unlock()

			case err := <-errCh:
				if err != nil {
					log.Error("election loop error", zap.Error(err))
				}
				goto end
			case <-s.stop:
				log.Debug("stopping election Loop")
				return
			}
		}
	end:
		time.Sleep(10 * time.Second)
	}
}

func (s *Sentinel) setSentinelInfo(ttl time.Duration) error {
	sentinelInfo := &cluster.SentinelInfo{
		UID: s.uid,
	}
	log.Debug("sentinelInfod dump", zap.String("sentinelInfo", spew.Sdump(sentinelInfo)))

	if err := s.e.SetSentinelInfo(sentinelInfo, ttl); err != nil {
		return err
	}
	return nil
}

func (s *Sentinel) getKeepersInfo(ctx context.Context) (cluster.KeepersInfo, error) {
	return s.e.GetKeepersInfo()
}

func (s *Sentinel) SetKeeperError(uid string) {
	if _, ok := s.keeperErrorTimers[uid]; !ok {
		s.keeperErrorTimers[uid] = timer.Now()
	}
}

func (s *Sentinel) CleanKeeperError(uid string) {
	if _, ok := s.keeperErrorTimers[uid]; ok {
		delete(s.keeperErrorTimers, uid)
	}
}

func (s *Sentinel) SetDBError(uid string) {
	if _, ok := s.dbErrorTimers[uid]; !ok {
		s.dbErrorTimers[uid] = timer.Now()
	}
}

func (s *Sentinel) CleanDBError(uid string) {
	if _, ok := s.dbErrorTimers[uid]; ok {
		delete(s.dbErrorTimers, uid)
	}
}

func (s *Sentinel) updateKeepersStatus(cd *cluster.ClusterData, keepersInfo cluster.KeepersInfo, firstRun bool) (*cluster.ClusterData, KeeperInfoHistories) {
	// Create a copy of cd
	cd = cd.DeepCopy()

	kihs := s.keeperInfoHistories.DeepCopy()

	// Remove keepers with wrong cluster UID
	tmpKeepersInfo := keepersInfo.DeepCopy()
	for _, ki := range keepersInfo {
		if ki.ClusterUID != cd.Cluster.UID {
			delete(tmpKeepersInfo, ki.UID)
		}
	}
	keepersInfo = tmpKeepersInfo

	// On first run just insert keepers info in the history with Seen set
	// to false and don't do any change to the keepers' state
	if firstRun {
		for keeperUID, ki := range keepersInfo {
			kihs[keeperUID] = &KeeperInfoHistory{KeeperInfo: ki, Seen: false}
		}
		return cd, kihs
	}

	tmpKeepersInfo = keepersInfo.DeepCopy()
	// keep only updated keepers info
	for keeperUID, ki := range keepersInfo {
		if kih, ok := kihs[keeperUID]; ok {
			log.Debug("kih", zap.Object("kih", kih))
			if kih.KeeperInfo.InfoUID == ki.InfoUID {
				if !kih.Seen {
					//Remove since it was already there and wasn't updated
					delete(tmpKeepersInfo, ki.UID)
				} else if kih.Seen && timer.Since(kih.Timer) > s.sleepInterval {
					//Remove since it wasn't updated
					delete(tmpKeepersInfo, ki.UID)
				}
			}
			if kih.KeeperInfo.InfoUID != ki.InfoUID {
				kihs[keeperUID] = &KeeperInfoHistory{KeeperInfo: ki, Seen: true, Timer: timer.Now()}
			}
		} else {
			kihs[keeperUID] = &KeeperInfoHistory{KeeperInfo: ki, Seen: true, Timer: timer.Now()}
		}
	}
	keepersInfo = tmpKeepersInfo

	// Create new keepers from keepersInfo
	for keeperUID, ki := range keepersInfo {
		if _, ok := cd.Keepers[keeperUID]; !ok {
			k := cluster.NewKeeperFromKeeperInfo(ki)
			cd.Keepers[k.UID] = k
		}
	}

	// Mark keepers without a keeperInfo (cleaned up above from not updated
	// ones) as in error
	for keeperUID, k := range cd.Keepers {
		if ki, ok := keepersInfo[keeperUID]; !ok {
			s.SetKeeperError(keeperUID)
		} else {
			s.CleanKeeperError(keeperUID)
			// Update keeper status infos
			k.Status.BootUUID = ki.BootUUID
		}
	}

	// Update keepers' healthy states
	for _, k := range cd.Keepers {
		k.Status.Healthy = s.isKeeperHealthy(cd, k)
	}

	// Update dbs' states
	for _, db := range cd.DBs {
		// Mark not found DBs in DBstates in error
		k, ok := keepersInfo[db.Spec.KeeperUID]
		if !ok {
			log.Error("no keeper info available", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
			s.SetDBError(db.UID)
			continue
		}
		dbs := k.PostgresState
		if dbs == nil {
			log.Error("no db state available", zap.String("db", db.UID))
			s.SetDBError(db.UID)
			continue
		}
		if dbs.UID != db.UID {
			log.Warn("received db state for unexpected db uid", zap.String("receivedDB", dbs.UID), zap.String("db", db.UID))
			s.SetDBError(db.UID)
			continue
		}
		log.Debug("received db state", zap.String("db", db.UID))
		db.Status.ListenAddress = dbs.ListenAddress
		db.Status.Port = dbs.Port
		db.Status.CurrentGeneration = dbs.Generation
		if dbs.Healthy {
			s.CleanDBError(db.UID)
			db.Status.SystemID = dbs.SystemID
			db.Status.TimelineID = dbs.TimelineID
			db.Status.XLogPos = dbs.XLogPos
			db.Status.TimelinesHistory = dbs.TimelinesHistory
			db.Status.PGParameters = cluster.PGParameters(dbs.PGParameters)
			// Sort synchronousStandbys so we can compare the slice regardless of its order
			sort.Sort(sort.StringSlice(dbs.SynchronousStandbys))
			db.Status.SynchronousStandbys = dbs.SynchronousStandbys
		} else {
			s.SetDBError(db.UID)
		}
	}

	// Update dbs' healthy state
	for _, db := range cd.DBs {
		db.Status.Healthy = s.isDBHealthy(cd, db)
	}

	return cd, kihs
}

func (s *Sentinel) findInitialKeeper(cd *cluster.ClusterData) (*cluster.Keeper, error) {
	if len(cd.Keepers) < 1 {
		return nil, fmt.Errorf("no keepers registered")
	}
	r := s.RandFn(len(cd.Keepers))
	keys := []string{}
	for k, _ := range cd.Keepers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return cd.Keepers[keys[r]], nil
}

func (s *Sentinel) setDBSpecFromClusterSpec(cd *cluster.ClusterData) {
	// Update dbSpec values with the related clusterSpec ones
	for _, db := range cd.DBs {
		db.Spec.RequestTimeout = *cd.Cluster.DefSpec().RequestTimeout
		db.Spec.MaxStandbys = *cd.Cluster.DefSpec().MaxStandbys
		db.Spec.SynchronousReplication = *cd.Cluster.DefSpec().SynchronousReplication
		db.Spec.UsePgrewind = *cd.Cluster.DefSpec().UsePgrewind
		db.Spec.PGParameters = cd.Cluster.DefSpec().PGParameters
	}
}

func (s *Sentinel) isDifferentTimelineBranch(followedDB *cluster.DB, db *cluster.DB) bool {
	if followedDB.Status.TimelineID < db.Status.TimelineID {
		log.Info("followed instance timeline < than our timeline", zap.Uint64("followedTimeline", followedDB.Status.TimelineID), zap.Uint64("timeline", db.Status.TimelineID))
		return true
	}

	// if the timelines are the same check that also the switchpoints are the same.
	if followedDB.Status.TimelineID == db.Status.TimelineID {
		if db.Status.TimelineID <= 1 {
			// if timeline <= 1 then no timeline history file exists.
			return false
		}
		ftlh := followedDB.Status.TimelinesHistory.GetTimelineHistory(db.Status.TimelineID - 1)
		tlh := db.Status.TimelinesHistory.GetTimelineHistory(db.Status.TimelineID - 1)
		if ftlh == nil || tlh == nil {
			// No timeline history to check
			return false
		}
		if ftlh.SwitchPoint == tlh.SwitchPoint {
			return false
		}
		log.Info("followed instance timeline forked at a different xlog pos than our timeline", zap.Uint64("followedTimeline", followedDB.Status.TimelineID), zap.Uint64("followedXlogpos", ftlh.SwitchPoint), zap.Uint64("timeline", db.Status.TimelineID), zap.Uint64("xlogpos", tlh.SwitchPoint))
		return true
	}

	// followedDB.Status.TimelineID > db.Status.TimelineID
	ftlh := followedDB.Status.TimelinesHistory.GetTimelineHistory(db.Status.TimelineID)
	if ftlh != nil {
		if ftlh.SwitchPoint < db.Status.XLogPos {
			log.Info("followed instance timeline forked before our current state", zap.Uint64("followedTimeline", followedDB.Status.TimelineID), zap.Uint64("followedXlogpos", ftlh.SwitchPoint), zap.Uint64("timeline", db.Status.TimelineID), zap.Uint64("xlogpos", db.Status.XLogPos))
			return true
		}
	}
	return false
}

func (s *Sentinel) freeKeepers(cd *cluster.ClusterData) []*cluster.Keeper {
	freeKeepers := []*cluster.Keeper{}
K:
	for _, keeper := range cd.Keepers {
		if !keeper.Status.Healthy {
			continue
		}
		for _, db := range cd.DBs {
			if db.Spec.KeeperUID == keeper.UID {
				continue K
			}
		}
		freeKeepers = append(freeKeepers, keeper)
	}
	return freeKeepers
}

type dbType int
type dbValidity int
type dbStatus int

const (
	// TODO(sgotti) change "master" and "standby" to different name to
	// better differentiate with with master and standby db roles.
	dbTypeMaster dbType = iota
	dbTypeStandby

	dbValidityValid dbValidity = iota
	dbValidityInvalid
	dbValidityUnknown

	dbStatusGood dbStatus = iota
	dbStatusFailed
	dbStatusConverging
)

// dbType returns the db type
// A master is a db that:
// * Has a master db role or a standby db role with followtype external
// A standby is a db that:
// * Has a standby db role with followtype internal
func (s *Sentinel) dbType(cd *cluster.ClusterData, dbUID string) dbType {
	db, ok := cd.DBs[dbUID]
	if !ok {
		panic(fmt.Errorf("requested unexisting db uid %q", dbUID))
	}
	switch db.Spec.Role {
	case common.RoleMaster:
		return dbTypeMaster
	case common.RoleStandby:
		if db.Spec.FollowConfig.Type == cluster.FollowTypeExternal {
			return dbTypeMaster
		}
		return dbTypeStandby
	default:
		panic("invalid db type in db Spec")
	}
}

// dbValidity return the validity of a db
// a db isn't valid when it has a different postgres systemdID or is on a
// different timeline branch
// dbs with CurrentGeneration == NoGeneration (0) are reported as
// dbValidityUnknown since the db status is empty.
func (s *Sentinel) dbValidity(cd *cluster.ClusterData, dbUID string) dbValidity {
	db, ok := cd.DBs[dbUID]
	if !ok {
		panic(fmt.Errorf("requested unexisting db uid %q", dbUID))
	}

	if db.Status.CurrentGeneration == cluster.NoGeneration {
		return dbValidityUnknown
	}

	masterDB := cd.DBs[cd.Cluster.Status.Master]

	// if with a different postgres systemID it's invalid
	if db.Status.SystemID != masterDB.Status.SystemID {
		log.Debug("invalid db since the postgres systemdID is different that the master one", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID), zap.String("dbSystemdID", db.Status.SystemID), zap.String("masterSystemID", masterDB.Status.SystemID))
		return dbValidityInvalid
	}

	// If on a different timeline branch it's invalid
	if s.isDifferentTimelineBranch(masterDB, db) {
		return dbValidityInvalid
	}

	// db is valid
	return dbValidityValid
}

func (s *Sentinel) dbStatus(cd *cluster.ClusterData, dbUID string) dbStatus {
	db, ok := cd.DBs[dbUID]
	if !ok {
		panic(fmt.Errorf("requested unexisting db uid %q", dbUID))
	}

	// if keeper failed then mark as failed
	keeper := cd.Keepers[db.Spec.KeeperUID]
	if !keeper.Status.Healthy {
		return dbStatusFailed
	}

	convergenceTimeout := cd.Cluster.DefSpec().ConvergenceTimeout.Duration
	// check if db should be in init mode and adjust convergence timeout
	if db.Generation == cluster.InitialGeneration {
		if db.Spec.InitMode == cluster.DBInitModeResync {
			convergenceTimeout = cd.Cluster.DefSpec().SyncTimeout.Duration
		}

	}
	convergenceState := s.dbConvergenceState(db, convergenceTimeout)
	switch convergenceState {
	// if convergence failed then mark as failed
	case ConvergenceFailed:
		return dbStatusFailed
	// if converging then it's not failed (it can also be not healthy since it could be resyncing)
	case Converging:
		return dbStatusConverging
	}
	// if converged but not healthy mark as failed
	if !db.Status.Healthy {
		return dbStatusFailed
	}

	// TODO(sgotti) Check that the standby is successfully syncing with the
	// master (there can be different reasons:
	// * standby cannot connect to the master (network problems)
	// * missing wal segments (this shouldn't happen while keeping the same
	// master since we aren't removing replication slots for the life of a
	// standbydb in the cluster data, but could happen when electing a new
	// master if the elected standby db cluster doesn't have all the wals)

	// db is good
	return dbStatusGood
}

func (s *Sentinel) validMastersByStatus(cd *cluster.ClusterData) (map[string]*cluster.DB, map[string]*cluster.DB, map[string]*cluster.DB) {
	goodMasters := map[string]*cluster.DB{}
	failedMasters := map[string]*cluster.DB{}
	convergingMasters := map[string]*cluster.DB{}

	for _, db := range cd.DBs {
		// keep only valid masters
		if s.dbValidity(cd, db.UID) != dbValidityValid || s.dbType(cd, db.UID) != dbTypeMaster {
			continue
		}
		status := s.dbStatus(cd, db.UID)
		switch status {
		case dbStatusGood:
			goodMasters[db.UID] = db
		case dbStatusFailed:
			failedMasters[db.UID] = db
		case dbStatusConverging:
			convergingMasters[db.UID] = db
		}
	}
	return goodMasters, failedMasters, convergingMasters
}

func (s *Sentinel) validStandbysByStatus(cd *cluster.ClusterData) (map[string]*cluster.DB, map[string]*cluster.DB, map[string]*cluster.DB) {
	goodStandbys := map[string]*cluster.DB{}
	failedStandbys := map[string]*cluster.DB{}
	convergingStandbys := map[string]*cluster.DB{}

	for _, db := range cd.DBs {
		// keep only valid standbys
		if s.dbValidity(cd, db.UID) != dbValidityValid || s.dbType(cd, db.UID) != dbTypeStandby {
			continue
		}
		status := s.dbStatus(cd, db.UID)
		switch status {
		case dbStatusGood:
			goodStandbys[db.UID] = db
		case dbStatusFailed:
			failedStandbys[db.UID] = db
		case dbStatusConverging:
			convergingStandbys[db.UID] = db
		}
	}
	return goodStandbys, failedStandbys, convergingStandbys
}

// dbSlice implements sort interface to sort by XLogPos
type dbSlice []*cluster.DB

func (p dbSlice) Len() int           { return len(p) }
func (p dbSlice) Less(i, j int) bool { return p[i].Status.XLogPos < p[j].Status.XLogPos }
func (p dbSlice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

func (s *Sentinel) findBestStandbys(cd *cluster.ClusterData, masterDB *cluster.DB) []*cluster.DB {
	goodStandbys, _, _ := s.validStandbysByStatus(cd)
	bestDBs := []*cluster.DB{}
	for _, db := range goodStandbys {
		if db.Status.TimelineID != masterDB.Status.TimelineID {
			log.Debug("ignoring keeper since its pg timeline is different than master timeline", zap.String("db", db.UID), zap.Uint64("dbTimeline", db.Status.TimelineID), zap.Uint64("masterTimeline", masterDB.Status.TimelineID))
			continue
		}
		bestDBs = append(bestDBs, db)
	}
	// Sort by XLogPos
	sort.Sort(dbSlice(bestDBs))
	return bestDBs
}

func (s *Sentinel) findBestNewMasters(cd *cluster.ClusterData, masterDB *cluster.DB) []*cluster.DB {
	bestNewMasters := s.findBestStandbys(cd, masterDB)
	// Add the previous masters to the best standbys (if valid and in good state)
	goodMasters, _, _ := s.validMastersByStatus(cd)
	log.Debug("goodMasters", zap.String("goodMasters", spew.Sdump(goodMasters)))
	for _, db := range goodMasters {
		if db.UID == masterDB.UID {
			log.Debug("ignoring db since it's the current master", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
			continue
		}
		if db.Status.TimelineID != masterDB.Status.TimelineID {
			log.Debug("ignoring keeper since its pg timeline is different than master timeline", zap.String("db", db.UID), zap.Uint64("dbTimeline", db.Status.TimelineID), zap.Uint64("masterTimeline", masterDB.Status.TimelineID))
			continue
		}
		bestNewMasters = append(bestNewMasters, db)
	}
	// Sort by XLogPos
	sort.Sort(dbSlice(bestNewMasters))
	log.Debug("bestNewMasters", zap.String("bestNewMasters", spew.Sdump(bestNewMasters)))
	return bestNewMasters
}

func (s *Sentinel) updateCluster(cd *cluster.ClusterData) (*cluster.ClusterData, error) {
	newcd := cd.DeepCopy()
	switch cd.Cluster.Status.Phase {
	case cluster.ClusterPhaseInitializing:
		switch *cd.Cluster.DefSpec().InitMode {
		case cluster.ClusterInitModeNew:
			// Is there already a keeper choosed to be the new master?
			if cd.Cluster.Status.Master == "" {
				log.Info("trying to find initial master")
				k, err := s.findInitialKeeper(cd)
				if err != nil {
					return nil, fmt.Errorf("cannot choose initial master: %v", err)
				}
				log.Info("initializing cluster", zap.String("keeper", k.UID))
				db := &cluster.DB{
					UID:        s.UIDFn(),
					Generation: cluster.InitialGeneration,
					ChangeTime: time.Now(),
					Spec: &cluster.DBSpec{
						KeeperUID:     k.UID,
						InitMode:      cluster.DBInitModeNew,
						Role:          common.RoleMaster,
						Followers:     []string{},
						IncludeConfig: *cd.Cluster.DefSpec().MergePgParameters,
					},
				}
				newcd.DBs[db.UID] = db
				newcd.Cluster.Status.Master = db.UID
				log.Debug("newcd dump", zap.String("newcd", spew.Sdump(newcd)))
			} else {
				db, ok := cd.DBs[cd.Cluster.Status.Master]
				if !ok {
					panic(fmt.Errorf("db %q object doesn't exists. This shouldn't happen", cd.Cluster.Status.Master))
				}
				// Check that the choosed db for being the master has correctly initialized
				switch s.dbConvergenceState(db, cd.Cluster.DefSpec().InitTimeout.Duration) {
				case Converged:
					if db.Status.Healthy {
						log.Info("db initialized", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
						// Set db initMode to none, not needed but just a security measure
						db.Spec.InitMode = cluster.DBInitModeNone
						// Don't include previous config anymore
						db.Spec.IncludeConfig = false
						// Replace reported pg parameters in cluster spec
						if *cd.Cluster.DefSpec().MergePgParameters {
							newcd.Cluster.Spec.PGParameters = db.Status.PGParameters
						}
						// Cluster initialized, switch to Normal state
						newcd.Cluster.Status.Phase = cluster.ClusterPhaseNormal
					}
				case Converging:
					log.Info("waiting for db", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
				case ConvergenceFailed:
					log.Info("db failed to initialize", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
					// Empty DBs
					newcd.DBs = cluster.DBs{}
					// Unset master so another keeper can be choosen
					newcd.Cluster.Status.Master = ""
				}
			}
		case cluster.ClusterInitModeExisting:
			if cd.Cluster.Status.Master == "" {
				wantedKeeper := cd.Cluster.DefSpec().ExistingConfig.KeeperUID
				log.Info("trying to use keeper as initial master", zap.String("keeper", wantedKeeper))

				k, ok := cd.Keepers[wantedKeeper]
				if !ok {
					return nil, fmt.Errorf("keeper %q state not available", wantedKeeper)
				}

				log.Info("initializing cluster using selected keeper as master db owner", zap.String("keeper", k.UID))

				db := &cluster.DB{
					UID:        s.UIDFn(),
					Generation: cluster.InitialGeneration,
					ChangeTime: time.Now(),
					Spec: &cluster.DBSpec{
						KeeperUID:     k.UID,
						InitMode:      cluster.DBInitModeExisting,
						Role:          common.RoleMaster,
						Followers:     []string{},
						IncludeConfig: *cd.Cluster.DefSpec().MergePgParameters,
					},
				}
				newcd.DBs[db.UID] = db
				newcd.Cluster.Status.Master = db.UID
				log.Debug("newcd dump", zap.String("newcd", spew.Sdump(newcd)))
			} else {
				db, ok := newcd.DBs[cd.Cluster.Status.Master]
				if !ok {
					panic(fmt.Errorf("db %q object doesn't exists. This shouldn't happen", cd.Cluster.Status.Master))
				}
				// Check that the choosed db for being the master has correctly initialized
				if db.Status.Healthy && s.dbConvergenceState(db, cd.Cluster.DefSpec().ConvergenceTimeout.Duration) == Converged {
					log.Info("db initialized", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
					// Don't include previous config anymore
					db.Spec.IncludeConfig = false
					// Replace reported pg parameters in cluster spec
					if *cd.Cluster.DefSpec().MergePgParameters {
						newcd.Cluster.Spec.PGParameters = db.Status.PGParameters
					}
					// Cluster initialized, switch to Normal state
					newcd.Cluster.Status.Phase = cluster.ClusterPhaseNormal
				}
			}
		case cluster.ClusterInitModePITR:
			// Is there already a keeper choosed to be the new master?
			if cd.Cluster.Status.Master == "" {
				log.Info("trying to find initial master")
				k, err := s.findInitialKeeper(cd)
				if err != nil {
					return nil, fmt.Errorf("cannot choose initial master: %v", err)
				}
				log.Info("initializing cluster using selected keeper as master db owner", zap.String("keeper", k.UID))
				db := &cluster.DB{
					UID:        s.UIDFn(),
					Generation: cluster.InitialGeneration,
					ChangeTime: time.Now(),
					Spec: &cluster.DBSpec{
						KeeperUID:     k.UID,
						InitMode:      cluster.DBInitModePITR,
						PITRConfig:    cd.Cluster.DefSpec().PITRConfig,
						Role:          common.RoleMaster,
						Followers:     []string{},
						IncludeConfig: *cd.Cluster.DefSpec().MergePgParameters,
					},
				}
				newcd.DBs[db.UID] = db
				newcd.Cluster.Status.Master = db.UID
				log.Debug("newcd dump", zap.String("newcd", spew.Sdump(newcd)))
			} else {
				db, ok := cd.DBs[cd.Cluster.Status.Master]
				if !ok {
					panic(fmt.Errorf("db %q object doesn't exists. This shouldn't happen", cd.Cluster.Status.Master))
				}
				// Check that the choosed db for being the master has correctly initialized
				// TODO(sgotti) set a timeout (the max time for a restore operation)
				switch s.dbConvergenceState(db, 0) {
				case Converged:
					if db.Status.Healthy {
						log.Info("db initialized", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
						// Set db initMode to none, not needed but just a security measure
						db.Spec.InitMode = cluster.DBInitModeNone
						// Don't include previous config anymore
						db.Spec.IncludeConfig = false
						// Replace reported pg parameters in cluster spec
						if *cd.Cluster.DefSpec().MergePgParameters {
							newcd.Cluster.Spec.PGParameters = db.Status.PGParameters
						}
						// Cluster initialized, switch to Normal state
						newcd.Cluster.Status.Phase = cluster.ClusterPhaseNormal
					}
				case Converging:
					log.Info("waiting for db to converge", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
				case ConvergenceFailed:
					log.Info("db failed to initialize", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
					// Empty DBs
					newcd.DBs = cluster.DBs{}
					// Unset master so another keeper can be choosen
					newcd.Cluster.Status.Master = ""
				}
			}
		default:
			return nil, fmt.Errorf("unknown init mode %q", cd.Cluster.DefSpec().InitMode)
		}
	case cluster.ClusterPhaseNormal:
		// TODO(sgotti) When keeper removal is implemented, remove DBs for unexistent keepers

		// Calculate current master status
		curMasterDBUID := cd.Cluster.Status.Master
		wantedMasterDBUID := curMasterDBUID

		masterOK := true
		curMasterDB := cd.DBs[curMasterDBUID]
		if curMasterDB == nil {
			return nil, fmt.Errorf("db for keeper %q not available. This shouldn't happen!", curMasterDBUID)
		}
		log.Debug("db dump", zap.String("db", spew.Sdump(curMasterDB)))

		if !curMasterDB.Status.Healthy {
			log.Info("master db is failed", zap.String("db", curMasterDB.UID), zap.String("keeper", curMasterDB.Spec.KeeperUID))
			masterOK = false
		}

		// Check that the wanted master is in master state (i.e. check that promotion from standby to master happened)
		if s.dbConvergenceState(curMasterDB, cd.Cluster.DefSpec().ConvergenceTimeout.Duration) == ConvergenceFailed {
			log.Info("db not converged", zap.String("db", curMasterDB.UID), zap.String("keeper", curMasterDB.Spec.KeeperUID))
			masterOK = false
		}

		if !masterOK {
			log.Info("trying to find a new master to replace failed master")
			bestNewMasters := s.findBestNewMasters(cd, curMasterDB)
			if len(bestNewMasters) == 0 {
				log.Error("no eligible masters")
			} else {
				// if synchronous replication is enabled, only choose new master in the synchronous replication standbys.
				var bestNewMasterDB *cluster.DB
				if *cd.Cluster.DefSpec().SynchronousReplication {
					onlyFake := true
					// if only fake synchronous standbys are defined we cannot choose any standby
					for _, dbUID := range curMasterDB.Spec.SynchronousStandbys {
						if dbUID != fakeStandbyName {
							onlyFake = false
						}
					}
					if !onlyFake {
						if !util.CompareStringSlice(curMasterDB.Status.SynchronousStandbys, curMasterDB.Spec.SynchronousStandbys) {
							log.Warn("cannot choose synchronous standby since the latest master reported synchronous standbys are different from the db spec ones", zap.Object("reported", spew.Sdump(curMasterDB.Status.SynchronousStandbys)), zap.Object("spec", spew.Sdump(curMasterDB.Spec.SynchronousStandbys)))
						} else {
							for _, nm := range bestNewMasters {
								if util.StringInSlice(curMasterDB.Spec.SynchronousStandbys, nm.UID) {
									bestNewMasterDB = nm
									break
								}
							}
						}
					}
				} else {
					bestNewMasterDB = bestNewMasters[0]
				}
				if bestNewMasterDB != nil {
					log.Info("electing db as the new master", zap.String("db", bestNewMasterDB.UID), zap.String("keeper", bestNewMasterDB.Spec.KeeperUID))
					wantedMasterDBUID = bestNewMasterDB.UID
				} else {
					log.Error("no eligible masters")
				}
			}
		}

		// New master elected
		if curMasterDBUID != wantedMasterDBUID {
			// maintain the current role, remove followers
			oldMasterdb := newcd.DBs[curMasterDBUID]
			oldMasterdb.Spec.Followers = []string{}

			newcd.Cluster.Status.Master = wantedMasterDBUID
			newMasterDB := newcd.DBs[wantedMasterDBUID]
			newMasterDB.Spec.Role = common.RoleMaster
			newMasterDB.Spec.FollowConfig = nil

			// Tell proxy that there's currently no active master
			newcd.Proxy.Spec.MasterDBUID = ""
			newcd.Proxy.ChangeTime = time.Now()

			// Setup synchronous standbys to the one of the previous master (replacing ourself with the previous master)
			if *cd.Cluster.DefSpec().SynchronousReplication {
				for _, dbUID := range oldMasterdb.Spec.SynchronousStandbys {
					newMasterDB.Spec.SynchronousStandbys = []string{}
					if dbUID != newMasterDB.UID {
						newMasterDB.Spec.SynchronousStandbys = append(newMasterDB.Spec.SynchronousStandbys, dbUID)
					} else {
						newMasterDB.Spec.SynchronousStandbys = append(newMasterDB.Spec.SynchronousStandbys, oldMasterdb.UID)
					}
				}
				if len(newMasterDB.Spec.SynchronousStandbys) == 0 {
					newMasterDB.Spec.SynchronousStandbys = []string{fakeStandbyName}
				}
			}
		}

		// TODO(sgotti) Wait for the proxies being converged (closed connections to old master)?

		// Setup standbys, do this only when there's no master change
		if curMasterDBUID == wantedMasterDBUID {
			masterDB := newcd.DBs[curMasterDBUID]
			// Set standbys to follow master only if it's healthy and converged
			if masterDB.Status.Healthy && s.dbConvergenceState(masterDB, cd.Cluster.DefSpec().ConvergenceTimeout.Duration) == Converged {
				// Tell proxy that there's a new active master
				newcd.Proxy.Spec.MasterDBUID = wantedMasterDBUID
				newcd.Proxy.ChangeTime = time.Now()

				// Remove old masters
				toRemove := []*cluster.DB{}
				for _, db := range newcd.DBs {
					if db.UID == wantedMasterDBUID {
						continue
					}
					if s.dbType(newcd, db.UID) != dbTypeMaster {
						continue
					}
					log.Info("removing old master db", zap.String("db", db.UID))
					toRemove = append(toRemove, db)
				}
				for _, db := range toRemove {
					delete(newcd.DBs, db.UID)
				}

				// Remove invalid dbs
				toRemove = []*cluster.DB{}
				for _, db := range newcd.DBs {
					if db.UID == wantedMasterDBUID {
						continue
					}
					if s.dbValidity(newcd, db.UID) != dbValidityInvalid {
						continue
					}
					log.Info("removing invalid db", zap.String("db", db.UID))
					toRemove = append(toRemove, db)
				}
				for _, db := range toRemove {
					delete(newcd.DBs, db.UID)
				}

				goodStandbys, failedStandbys, convergingStandbys := s.validStandbysByStatus(newcd)
				goodStandbysCount := len(goodStandbys)
				failedStandbysCount := len(failedStandbys)
				convergingStandbysCount := len(convergingStandbys)
				log.Debug("standbys states", zap.Int("good", goodStandbysCount), zap.Int("failed", failedStandbysCount), zap.Int("converging", convergingStandbysCount))

				// Setup synchronous standbys
				if *cd.Cluster.DefSpec().SynchronousReplication {
					// make a map of synchronous standbys starting from the current ones
					synchronousStandbys := map[string]struct{}{}
					for _, dbUID := range masterDB.Spec.SynchronousStandbys {
						// filter out fake standby
						if dbUID == fakeStandbyName {
							continue
						}
						synchronousStandbys[dbUID] = struct{}{}
					}

					// Check if the current synchronous standbys are healthy or remove them
					toRemove := map[string]struct{}{}
					for dbUID, _ := range synchronousStandbys {
						if _, ok := goodStandbys[dbUID]; !ok {
							log.Info("removing failed synchronous standby", zap.String("masterDB", masterDB.UID), zap.String("db", dbUID))
							toRemove[dbUID] = struct{}{}
						}
					}
					for dbUID, _ := range toRemove {
						delete(synchronousStandbys, dbUID)
					}

					// Remove synchronous standbys in excess
					if uint16(len(synchronousStandbys)) > *cd.Cluster.DefSpec().MaxSynchronousStandbys {
						rc := len(synchronousStandbys) - int(*cd.Cluster.DefSpec().MaxSynchronousStandbys)
						removedCount := 0
						toRemove = map[string]struct{}{}
						for dbUID, _ := range synchronousStandbys {
							if removedCount >= rc {
								break
							}
							log.Info("removing synchronous standby in excess", zap.String("masterDB", masterDB.UID), zap.String("db", dbUID))
							toRemove[dbUID] = struct{}{}
							removedCount++
						}
						for dbUID, _ := range toRemove {
							delete(synchronousStandbys, dbUID)
						}
					}

					// try to add missing standbys up to *cd.Cluster.DefSpec().MaxSynchronousStandbys
					bestStandbys := s.findBestStandbys(newcd, curMasterDB)
					ac := int(*cd.Cluster.DefSpec().MaxSynchronousStandbys) - len(synchronousStandbys)
					addedCount := 0
					for _, bestStandby := range bestStandbys {
						if addedCount >= ac {
							break
						}
						if _, ok := synchronousStandbys[bestStandby.UID]; ok {
							continue
						}
						log.Info("adding synchronous standby", zap.String("masterDB", masterDB.UID), zap.String("synchronousStandbyDB", bestStandby.UID))
						synchronousStandbys[bestStandby.UID] = struct{}{}
						addedCount++
					}

					// If there're not enough real synchronous standbys add a fake synchronous standby because we have to be strict and make the master block transactions until MaxSynchronousStandbys real standbys are available
					if len(synchronousStandbys) < int(*cd.Cluster.DefSpec().MinSynchronousStandbys) {
						log.Info("using a fake synchronous standby since there are not enough real standbys available", zap.String("masterDB", masterDB.UID), zap.Int("required", int(*cd.Cluster.DefSpec().MinSynchronousStandbys)))
						synchronousStandbys[fakeStandbyName] = struct{}{}
					}

					masterDB.Spec.SynchronousStandbys = []string{}
					for dbUID, _ := range synchronousStandbys {
						masterDB.Spec.SynchronousStandbys = append(masterDB.Spec.SynchronousStandbys, dbUID)
					}

					// Sort synchronousStandbys so we can compare the slice regardless of its order
					sort.Sort(sort.StringSlice(masterDB.Spec.SynchronousStandbys))
				}

				// NotFailed != Good since there can be some dbs that are converging
				// it's the total number of standbys - the failed standbys
				// or the sum of good + converging standbys
				notFailedStandbysCount := goodStandbysCount + convergingStandbysCount

				// Remove dbs in excess if we have a good number >= MaxStandbysPerSender
				if uint16(goodStandbysCount) >= *cd.Cluster.DefSpec().MaxStandbysPerSender {
					toRemove := []*cluster.DB{}
					// Remove all non good standbys
					for _, db := range newcd.DBs {
						if s.dbType(newcd, db.UID) != dbTypeStandby {
							continue
						}
						if _, ok := goodStandbys[db.UID]; !ok {
							log.Info("removing non good standby", zap.String("db", db.UID))
							toRemove = append(toRemove, db)
						}
					}
					// Remove good standbys in excess
					nr := int(uint16(goodStandbysCount) - *cd.Cluster.DefSpec().MaxStandbysPerSender)
					i := 0
					for _, db := range goodStandbys {
						if i >= nr {
							break
						}
						// Don't remove standbys marked as synchronous standbys
						if util.StringInSlice(masterDB.Spec.SynchronousStandbys, db.UID) {
							continue
						}
						log.Info("removing good standby in excess", zap.String("db", db.UID))
						toRemove = append(toRemove, db)
						i++
					}
					for _, db := range toRemove {
						delete(newcd.DBs, db.UID)
					}

				} else {
					// Add new dbs to substitute failed dbs. we
					// don't remove failed db until the number of
					// good db is >= MaxStandbysPerSender since they can come back

					// define, if there're available keepers, new dbs
					// nc can be negative if MaxStandbysPerSender has been lowered
					nc := int(*cd.Cluster.DefSpec().MaxStandbysPerSender - uint16(notFailedStandbysCount))
					// Add missing DBs until MaxStandbysPerSender
					freeKeepers := s.freeKeepers(newcd)
					nf := len(freeKeepers)
					for i := 0; i < nc && i < nf; i++ {
						freeKeeper := freeKeepers[i]
						db := &cluster.DB{
							UID:        s.UIDFn(),
							Generation: cluster.InitialGeneration,
							ChangeTime: time.Now(),
							Spec: &cluster.DBSpec{
								KeeperUID:    freeKeeper.UID,
								InitMode:     cluster.DBInitModeResync,
								Role:         common.RoleStandby,
								Followers:    []string{},
								FollowConfig: &cluster.FollowConfig{Type: cluster.FollowTypeInternal, DBUID: wantedMasterDBUID},
							},
						}
						newcd.DBs[db.UID] = db
						log.Info("added new standby db", zap.String("db", db.UID), zap.String("keeper", db.Spec.KeeperUID))
					}
				}

				// Reconfigure all standbys as followers of the current master
				for _, db := range newcd.DBs {
					if s.dbType(newcd, db.UID) != dbTypeStandby {
						continue
					}

					db.Spec.Role = common.RoleStandby
					// Remove followers
					db.Spec.Followers = []string{}
					db.Spec.FollowConfig = &cluster.FollowConfig{Type: cluster.FollowTypeInternal, DBUID: wantedMasterDBUID}
				}

				// Set followers for master DB
				masterDB.Spec.Followers = []string{}
				for _, db := range newcd.DBs {
					if masterDB.UID == db.UID {
						continue
					}
					fc := db.Spec.FollowConfig
					if fc != nil {
						if fc.Type == cluster.FollowTypeInternal && fc.DBUID == wantedMasterDBUID {
							masterDB.Spec.Followers = append(masterDB.Spec.Followers, db.UID)
						}
					}
				}
				// Sort followers so the slice won't be considered changed due to different order of the same entries.
				sort.Strings(masterDB.Spec.Followers)
			}
		}

	default:
		return nil, fmt.Errorf("unknown cluster phase %s", cd.Cluster.Status.Phase)
	}

	// Copy the clusterSpec parameters to the dbSpec
	s.setDBSpecFromClusterSpec(newcd)

	// Update generation on DBs if they have changed
	for dbUID, db := range newcd.DBs {
		prevDB, ok := cd.DBs[dbUID]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(db.Spec, prevDB.Spec) {
			log.Debug("db spec changed, updating generation", zap.String("prevDB", spew.Sdump(prevDB.Spec)), zap.String("db", spew.Sdump(db.Spec)))
			db.Generation++
			db.ChangeTime = time.Now()
		}
	}

	return newcd, nil
}

type ConvergenceState uint

const (
	Converging ConvergenceState = iota
	Converged
	ConvergenceFailed
)

func (s *Sentinel) isKeeperHealthy(cd *cluster.ClusterData, keeper *cluster.Keeper) bool {
	t, ok := s.keeperErrorTimers[keeper.UID]
	if !ok {
		return true
	}
	if timer.Since(t) > cd.Cluster.DefSpec().FailInterval.Duration {
		return false
	}
	return true
}

func (s *Sentinel) isDBHealthy(cd *cluster.ClusterData, db *cluster.DB) bool {
	t, ok := s.dbErrorTimers[db.UID]
	if !ok {
		return true
	}
	if timer.Since(t) > cd.Cluster.DefSpec().FailInterval.Duration {
		return false
	}
	return true
}

func (s *Sentinel) updateDBConvergenceInfos(cd *cluster.ClusterData) {
	for _, db := range cd.DBs {
		if db.Status.CurrentGeneration == db.Generation {
			delete(s.dbConvergenceInfos, db.UID)
			continue
		}
		nd := &DBConvergenceInfo{Generation: db.Generation, Timer: timer.Now()}
		d, ok := s.dbConvergenceInfos[db.UID]
		if !ok {
			s.dbConvergenceInfos[db.UID] = nd
		} else if d.Generation != db.Generation {
			s.dbConvergenceInfos[db.UID] = nd
		}
	}
}

func (s *Sentinel) dbConvergenceState(db *cluster.DB, timeout time.Duration) ConvergenceState {
	if db.Status.CurrentGeneration == db.Generation {
		return Converged
	}
	if timeout != 0 {
		d, ok := s.dbConvergenceInfos[db.UID]
		if !ok {
			panic(fmt.Errorf("no db convergence info for db %q, this shouldn't happen!", db.UID))
		}
		if timer.Since(d.Timer) > timeout {
			return ConvergenceFailed
		}
	}
	return Converging
}

type KeeperInfoHistory struct {
	KeeperInfo *cluster.KeeperInfo
	Seen       bool
	Timer      int64
}

type KeeperInfoHistories map[string]*KeeperInfoHistory

func (k KeeperInfoHistories) DeepCopy() KeeperInfoHistories {
	if k == nil {
		return nil
	}
	nk, err := copystructure.Copy(k)
	if err != nil {
		panic(err)
	}
	if !reflect.DeepEqual(k, nk) {
		panic("not equal")
	}
	return nk.(KeeperInfoHistories)
}

type DBConvergenceInfo struct {
	Generation int64
	Timer      int64
}

type Sentinel struct {
	uid string
	cfg *config
	e   *store.StoreManager

	candidate *leadership.Candidate
	stop      chan bool
	end       chan bool

	lastLeadershipCount uint

	updateMutex sync.Mutex
	leader      bool
	// Used to determine if we lost and regained the leadership
	leadershipCount uint
	leaderMutex     sync.Mutex

	initialClusterSpec *cluster.ClusterSpec

	sleepInterval  time.Duration
	requestTimeout time.Duration

	// Make UIDFn settable to ease testing with reproducible UIDs
	UIDFn func() string
	// Make RandFn settable to ease testing with reproducible "random" numbers
	RandFn func(int) int

	keeperErrorTimers  map[string]int64
	dbErrorTimers      map[string]int64
	dbConvergenceInfos map[string]*DBConvergenceInfo

	keeperInfoHistories KeeperInfoHistories
}

func NewSentinel(uid string, cfg *config, stop chan bool, end chan bool) (*Sentinel, error) {
	var initialClusterSpec *cluster.ClusterSpec
	if cfg.initialClusterSpecFile != "" {
		configData, err := ioutil.ReadFile(cfg.initialClusterSpecFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read provided initial cluster config file: %v", err)
		}
		if err := json.Unmarshal(configData, &initialClusterSpec); err != nil {
			return nil, fmt.Errorf("cannot parse provided initial cluster config: %v", err)
		}
		log.Debug("initialClusterSpec dump", zap.String("initialClusterSpec", spew.Sdump(initialClusterSpec)))
		if err := initialClusterSpec.Validate(); err != nil {
			return nil, fmt.Errorf("invalid initial cluster: %v", err)
		}
	}

	storePath := filepath.Join(common.StoreBasePath, cfg.clusterName)

	kvstore, err := store.NewStore(store.Config{
		Backend:       store.Backend(cfg.storeBackend),
		Endpoints:     cfg.storeEndpoints,
		CertFile:      cfg.storeCertFile,
		KeyFile:       cfg.storeKeyFile,
		CAFile:        cfg.storeCAFile,
		SkipTLSVerify: cfg.storeSkipTlsVerify,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot create store: %v", err)
	}
	e := store.NewStoreManager(kvstore, storePath)

	candidate := leadership.NewCandidate(kvstore, filepath.Join(storePath, common.SentinelLeaderKey), uid, store.MinTTL)

	return &Sentinel{
		uid:                uid,
		cfg:                cfg,
		e:                  e,
		candidate:          candidate,
		leader:             false,
		initialClusterSpec: initialClusterSpec,
		stop:               stop,
		end:                end,
		UIDFn:              common.UID,
		// This is just to choose a pseudo random keeper so
		// use math.rand (no need for crypto.rand) without an
		// initial seed.
		RandFn: rand.Intn,

		sleepInterval:  cluster.DefaultSleepInterval,
		requestTimeout: cluster.DefaultRequestTimeout,
	}, nil
}

func (s *Sentinel) Start() {
	endCh := make(chan struct{})

	ctx, cancel := context.WithCancel(context.Background())
	timerCh := time.NewTimer(0).C

	go s.electionLoop()

	for true {
		select {
		case <-s.stop:
			log.Debug("stopping stolon sentinel")
			cancel()
			s.candidate.Stop()
			s.end <- true
			return
		case <-timerCh:
			go func() {
				s.clusterSentinelCheck(ctx)
				endCh <- struct{}{}
			}()
		case <-endCh:
			timerCh = time.NewTimer(s.sleepInterval).C
		}
	}
}

func (s *Sentinel) leaderInfo() (bool, uint) {
	s.leaderMutex.Lock()
	defer s.leaderMutex.Unlock()
	return s.leader, s.leadershipCount
}

func (s *Sentinel) clusterSentinelCheck(pctx context.Context) {
	s.updateMutex.Lock()
	defer s.updateMutex.Unlock()
	e := s.e

	cd, prevCDPair, err := e.GetClusterData()
	if err != nil {
		log.Error("error retrieving cluster data", zap.Error(err))
		return
	}
	if cd != nil {
		if cd.FormatVersion != cluster.CurrentCDFormatVersion {
			log.Error("unsupported clusterdata format version", zap.Uint64("version", cd.FormatVersion))
			return
		}
		if err = cd.Cluster.Spec.Validate(); err != nil {
			log.Error("clusterdata validation failed", zap.Error(err))
			return
		}
		if cd.Cluster != nil {
			s.sleepInterval = cd.Cluster.DefSpec().SleepInterval.Duration
			s.requestTimeout = cd.Cluster.DefSpec().RequestTimeout.Duration
		}

	}

	log.Debug("cd dump", zap.String("cd", spew.Sdump(cd)))

	if cd == nil {
		// Cluster first initialization
		if s.initialClusterSpec == nil {
			log.Info("no cluster data available, waiting for it to appear")
			return
		}
		c := cluster.NewCluster(s.UIDFn(), s.initialClusterSpec)
		log.Info("writing initial cluster data")
		newcd := cluster.NewClusterData(c)
		log.Debug("newcd dump", zap.String("newcd", spew.Sdump(newcd)))
		if _, err = e.AtomicPutClusterData(newcd, nil); err != nil {
			log.Error("error saving cluster data", zap.Error(err))
		}
		return
	}

	if err = s.setSentinelInfo(2 * s.sleepInterval); err != nil {
		log.Error("cannot update sentinel info", zap.Error(err))
		return
	}

	ctx, cancel := context.WithTimeout(pctx, s.requestTimeout)
	keepersInfo, err := s.getKeepersInfo(ctx)
	cancel()
	if err != nil {
		log.Error("err", zap.Error(err))
		return
	}
	log.Debug("keepersInfo dump", zap.String("keepersInfo", spew.Sdump(keepersInfo)))

	isLeader, leadershipCount := s.leaderInfo()
	if !isLeader {
		return
	}

	// detect if this is the first check after (re)gaining leadership
	firstRun := false
	if s.lastLeadershipCount != leadershipCount {
		firstRun = true
		s.lastLeadershipCount = leadershipCount
	}

	// if this is the first check after (re)gaining leadership reset all
	// the internal timers
	if firstRun {
		s.keeperErrorTimers = make(map[string]int64)
		s.dbErrorTimers = make(map[string]int64)
		s.keeperInfoHistories = make(KeeperInfoHistories)
		s.dbConvergenceInfos = make(map[string]*DBConvergenceInfo)

		// Update db convergence timers since its the first run
		s.updateDBConvergenceInfos(cd)
	}

	newcd, newKeeperInfoHistories := s.updateKeepersStatus(cd, keepersInfo, firstRun)

	newcd, err = s.updateCluster(newcd)
	if err != nil {
		log.Error("failed to update cluster data", zap.Error(err))
		return
	}
	log.Debug("newcd dump after updateCluster", zap.String("newcd", spew.Sdump(newcd)))

	if newcd != nil {
		if _, err := e.AtomicPutClusterData(newcd, prevCDPair); err != nil {
			log.Error("error saving clusterdata", zap.Error(err))
		}
	}

	// Save the new keeperInfoHistories only on successfull cluster data
	// update or in the next run we'll think that the saved keeperInfo was
	// already applied.
	s.keeperInfoHistories = newKeeperInfoHistories

	// Update db convergence timers using the new cluster data
	s.updateDBConvergenceInfos(newcd)
}

func sigHandler(sigs chan os.Signal, stop chan bool) {
	s := <-sigs
	log.Debug("got signal", zap.Stringer("signal", s))
	close(stop)
}

func main() {
	flagutil.SetFlagsFromEnv(cmdSentinel.PersistentFlags(), "STSENTINEL")

	cmdSentinel.Execute()
}

func sentinel(cmd *cobra.Command, args []string) {
	if cfg.debug {
		log.SetLevel(zap.DebugLevel)
	}
	if cfg.clusterName == "" {
		die("cluster name required")
	}
	if cfg.storeBackend == "" {
		die("store backend type required")
	}

	uid := common.UID()
	log.Info("sentinel uid", zap.String("uid", uid))

	stop := make(chan bool, 0)
	end := make(chan bool, 0)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go sigHandler(sigs, stop)

	s, err := NewSentinel(uid, &cfg, stop, end)
	if err != nil {
		die("cannot create sentinel: %v", err)
	}
	go s.Start()

	<-end
}
