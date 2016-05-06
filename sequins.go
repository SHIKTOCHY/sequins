package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nightlyone/lockfile"
	"github.com/tylerb/graceful"

	"github.com/stripe/sequins/backend"
)

var errDirLocked = errors.New("failed to acquire lock")

type sequins struct {
	config  sequinsConfig
	http    *http.Server
	backend backend.Backend

	dbs     map[string]*db
	dbsLock sync.RWMutex

	peers       *peers
	zkWatcher   *zkWatcher
	proxyClient *http.Client

	refreshLock    sync.Mutex
	refreshWorkers chan bool
	refreshTicker  *time.Ticker
	sighups        chan os.Signal

	storeLock lockfile.Lockfile
}

func newSequins(backend backend.Backend, config sequinsConfig) *sequins {
	return &sequins{
		config:      config,
		backend:     backend,
		refreshLock: sync.Mutex{},
	}
}

func (s *sequins) init() error {
	if s.config.ZK.Servers != nil {
		err := s.initCluster()
		if err != nil {
			return err
		}
	}

	// Create local directories, and load any cached versions we have.
	err := s.initLocalStore()
	if err != nil {
		return fmt.Errorf("error initializing local store: %s", err)
	}

	// To limit the number of parallel loads, create a full buffered channel.
	// Workers grab one from the channel when they trigger a load, and put it
	// back when they're done.
	maxLoads := s.config.MaxParallelLoads
	if maxLoads != 0 {
		s.refreshWorkers = make(chan bool, maxLoads)
		for i := 0; i < maxLoads; i++ {
			s.refreshWorkers <- true
		}
	}

	// Trigger loads before we start up.
	s.refreshAll()
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()

	// Automatically refresh, if configured to do so.
	refresh := s.config.RefreshPeriod.Duration
	if refresh != 0 {
		s.refreshTicker = time.NewTicker(refresh)
		go func() {
			log.Println("Automatically checking for new versions every", refresh.String())
			for range s.refreshTicker.C {
				s.refreshAll()
			}
		}()
	}

	// Refresh on SIGHUP.
	sighups := make(chan os.Signal)
	signal.Notify(sighups, syscall.SIGHUP)
	go func() {
		for range sighups {
			s.refreshAll()
		}
	}()

	s.sighups = sighups
	return nil
}

func (s *sequins) initCluster() error {
	prefix := path.Join("/", s.config.ZK.ClusterName)
	zkWatcher, err := connectZookeeper(s.config.ZK.Servers, prefix,
		s.config.ZK.ConnectTimeout.Duration, s.config.ZK.SessionTimeout.Duration)
	if err != nil {
		return err
	}

	hostname := s.config.ZK.AdvertisedHostname
	if hostname == "" {
		hostname, err = os.Hostname()
		if err != nil {
			return err
		}
	}

	_, port, err := net.SplitHostPort(s.config.Bind)
	if err != nil {
		return err
	}

	routableAddress := net.JoinHostPort(hostname, port)
	shardID := s.config.ZK.ShardID
	if shardID == "" {
		shardID = routableAddress
	}

	peers := watchPeers(zkWatcher, shardID, routableAddress)
	peers.waitToConverge(s.config.ZK.TimeToConverge.Duration)

	s.zkWatcher = zkWatcher
	s.peers = peers
	s.proxyClient = &http.Client{Timeout: s.config.ZK.ProxyTimeout.Duration}
	return nil
}

func (s *sequins) initLocalStore() error {
	dataPath := filepath.Join(s.config.LocalStore, "data")
	err := os.MkdirAll(dataPath, 0755|os.ModeDir)
	if err != nil {
		return err
	}

	lock, _ := lockfile.New(filepath.Join(s.config.LocalStore, "sequins.lock"))
	s.storeLock = lock
	err = s.storeLock.TryLock()
	if err != nil {
		p, err := s.storeLock.GetOwner()
		if err == nil {
			log.Printf("The local store at %s is locked by process %d", s.config.LocalStore, p.Pid)
		}

		return errDirLocked
	}

	return nil
}

func (s *sequins) start() {
	defer s.shutdown()

	var h http.Handler = s
	if s.config.Debug.Bind != "" && s.config.Debug.Expvars {
		h = trackQueries(s)
	}

	log.Println("Listening on", s.config.Bind)
	graceful.Run(s.config.Bind, time.Second, h)
}

func (s *sequins) shutdown() {
	log.Println("Shutting down...")
	signal.Stop(s.sighups)

	if s.refreshTicker != nil {
		s.refreshTicker.Stop()
	}

	zk := s.zkWatcher
	if zk != nil {
		zk.close()
	}

	// TODO: figure out how to cancel in-progress downloads
	// s.dbsLock.Lock()
	// defer s.dbsLock.Unlock()

	// for _, db := range s.dbs {
	// 	db.close()
	// }

	s.storeLock.Unlock()
}

func (s *sequins) refreshAll() {
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()

	dbs, err := s.backend.ListDBs()
	if err != nil {
		log.Printf("Error listing DBs from %s: %s", s.backend.DisplayPath(""), err)
		return
	}

	// Add any new DBS, and trigger refreshes for existing ones. This
	// Lock pattern is a bit goofy, since the lock is on the request
	// path and we want to hold it for as short a time as possible.
	s.dbsLock.RLock()

	newDBs := make(map[string]*db)
	var backfills sync.WaitGroup
	for _, name := range dbs {
		db := s.dbs[name]
		if db == nil {
			db = newDB(s, name)
			backfills.Add(1)
			go func() {
				db.backfillVersions()
				backfills.Done()
			}()
		}

		newDBs[name] = db
	}

	backfills.Wait()
	for _, db := range newDBs {
		go s.refresh(db)
	}

	// Now, grab the full lock to switch the new map in.
	s.dbsLock.RUnlock()
	s.dbsLock.Lock()

	oldDBs := s.dbs
	s.dbs = newDBs

	// Finally, close any dbs that we're removing.
	s.dbsLock.Unlock()
	s.dbsLock.RLock()

	for name, db := range oldDBs {
		if s.dbs[name] == nil {
			db.close()
			db.delete()
		}
	}

	s.dbsLock.RUnlock()
}

func (s *sequins) refresh(db *db) {
	if s.refreshWorkers != nil {
		<-s.refreshWorkers
		defer func() {
			s.refreshWorkers <- true
		}()
	}

	err := db.refresh()
	if err != nil {
		log.Printf("Error refreshing %s: %s", db.name, err)
	}
}

func (s *sequins) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if r.URL.Path == "/" {
		s.serveStatus(w, r)
		return
	}

	var dbName, key string
	path := strings.TrimPrefix(r.URL.Path, "/")
	split := strings.Index(path, "/")
	if split == -1 {
		dbName = path
		key = ""
	} else {
		dbName = path[:split]
		key = path[split+1:]
	}

	if dbName == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.dbsLock.RLock()
	db := s.dbs[dbName]
	s.dbsLock.RUnlock()

	// If this is a proxy request, we don't want to confuse "we don't have this
	// db" with "we don't have this key"; if this is a proxied request, then the
	// peer apparently does have the db, and thinks we do too (which should never
	// happen). So we use 501 Not Implemented to indicate the former. To users,
	// we present a uniform 404.
	if db == nil {
		if r.URL.Query().Get("proxy") != "" {
			w.WriteHeader(http.StatusNotImplemented)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}

		return
	}

	db.serveKey(w, r, key)
}
