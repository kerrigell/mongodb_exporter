// Copyright 2017 Percona LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"gopkg.in/mgo.v2"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/percona/mongodb_exporter/collector"
	"github.com/percona/mongodb_exporter/shared"
	pmmVersion "github.com/percona/pmm/version"
	"gopkg.in/ini.v1"
)

const (
	program           = "mongodb_exporter"
	versionDataFormat = "20060102-15:04:05"
)

var (
	listenAddressF = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9216").String()
	metricsPathF   = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()

	collectDatabaseF   = kingpin.Flag("collect.database", "Enable collection of Database metrics").Bool()
	collectCollectionF = kingpin.Flag("collect.collection", "Enable collection of Collection metrics").Bool()
	collectTopF        = kingpin.Flag("collect.topmetrics", "Enable collection of table top metrics").Bool()
	collectIndexUsageF = kingpin.Flag("collect.indexusage", "Enable collection of per index usage stats").Bool()

	uriF = kingpin.Flag("mongodb.uri", "MongoDB URI, format").
		PlaceHolder("[mongodb://][user:pass@]host1[:port1][,host2[:port2],...][/database][?options]").
		Default("mongodb://localhost:27017").
		Envar("MONGODB_URI").
		String()

	authDB   = kingpin.Flag("mongodb.authentification-database", "Specifies the database in which the user is created").Default("").String()
	tlsF     = kingpin.Flag("mongodb.tls", "Enable tls connection with mongo server").Bool()
	tlsCertF = kingpin.Flag("mongodb.tls-cert", "Path to PEM file that contains the certificate (and optionally also the decrypted private key in PEM format).\n"+
		"    \tThis should include the whole certificate chain.\n"+
		"    \tIf provided: The connection will be opened via TLS to the MongoDB server.").Default("").String()
	tlsPrivateKeyF = kingpin.Flag("mongodb.tls-private-key", "Path to PEM file that contains the decrypted private key (if not contained in mongodb.tls-cert file).").Default("").String()
	tlsCAF         = kingpin.Flag("mongodb.tls-ca", "Path to PEM file that contains the CAs that are trusted for server connections.\n"+
		"    \tIf provided: MongoDB servers connecting to should present a certificate signed by one of this CAs.\n"+
		"    \tIf not provided: System default CAs are used.").Default("").String()
	tlsDisableHostnameValidationF = kingpin.Flag("mongodb.tls-disable-hostname-validation", "Disable hostname validation for server connection.").Bool()
	maxConnectionsF               = kingpin.Flag("mongodb.max-connections", "Max number of pooled connections to the database.").Default("1").Int()
	testF                         = kingpin.Flag("test", "Check MongoDB connection, print buildInfo() information and exit.").Bool()

	socketTimeoutF = kingpin.Flag("mongodb.socket-timeout", "Amount of time to wait for a non-responding socket to the database before it is forcefully closed.\n"+
		"    \tValid time units are 'ns', 'us' (or 'µs'), 'ms', 's', 'm', 'h'.").Default("3s").Duration()
	syncTimeoutF = kingpin.Flag("mongodb.sync-timeout", "Amount of time an operation with this session will wait before returning an error in case\n"+
		"    \ta connection to a usable server can't be established.\n"+
		"    \tValid time units are 'ns', 'us' (or 'µs'), 'ms', 's', 'm', 'h'.").Default("1m").Duration()

	// FIXME currently ignored
	// enabledGroupsFlag = flag.String("groups.enabled", "asserts,durability,background_flushing,connections,extra_info,global_lock,index_counters,network,op_counters,op_counters_repl,memory,locks,metrics", "Comma-separated list of groups to use, for more info see: docs.mongodb.org/manual/reference/command/serverStatus/")
	enabledGroupsFlag = kingpin.Flag("groups.enabled", "Currently ignored").Default("").String()

	//
	configCnf           = kingpin.Flag("config.mongodb-cnf", "Path to .mongo.cnf file to read Mongodb credentials from.").Default(path.Join(os.Getenv("HOME"), ".mongo.cnf")).String()
	defaultConnUser     = ""
	defaultConnPassword = ""

	gathersCache = newMongoGatherersCache()
)

var landingPage = []byte(`<html>
<head><title>MySQLd exporter</title></head>
<body>
<h1>mongodb exporter</h1>
<p><a href='` + *metricsPathF + `'>Metrics</a></p>
</body>
</html>
`)

func main() {
	initVersionInfo()
	log.AddFlags(kingpin.CommandLine)
	kingpin.Parse()

	if *testF {
		buildInfo, err := shared.TestConnection(
			shared.MongoSessionOpts{
				URI:                   *uriF,
				TLSConnection:         *tlsF,
				TLSCertificateFile:    *tlsCertF,
				TLSPrivateKeyFile:     *tlsPrivateKeyF,
				TLSCaFile:             *tlsCAF,
				TLSHostnameValidation: !(*tlsDisableHostnameValidationF),
				AuthentificationDB:    *authDB,
			},
		)
		if err != nil {
			log.Errorf("Can't connect to MongoDB: %s", err)
			os.Exit(1)
		}
		fmt.Println(string(buildInfo))
		os.Exit(0)
	}

	// Check mongo.cnf, Get default user credentials
	_, err := os.Stat(*configCnf)
	if err == nil {
		log.Infoln("parse mongodb config", *configCnf)
		if err = parseMongodbCnf(*configCnf); err != nil {
			log.Errorln("parse mongo config", err)
		}
	}

	// TODO: Maybe we should move version.Info() and version.BuildContext() to https://github.com/percona/exporter_shared
	// See: https://jira.percona.com/browse/PMM-3250 and https://github.com/percona/mongodb_exporter/pull/132#discussion_r262227248
	log.Infoln("Starting", program, version.Info())
	log.Infoln("Build context", version.BuildContext())

	programCollector := version.NewCollector(program)
	prometheus.MustRegister(programCollector)

	handlerFunc := newHandler()
	http.Handle(*metricsPathF, handlerFunc)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(landingPage)
	})

	log.Infoln("msg", "Listening on address", "address", *listenAddressF)
	if err := http.ListenAndServe(*listenAddressF, nil); err != nil {
		log.Infoln("msg", "Error starting HTTP server", "err", err)
		os.Exit(1)
	}
}

// initVersionInfo sets version info
// If binary was build for PMM with environment variable PMM_RELEASE_VERSION
// `--version` will be displayed in PMM format. Also `PMM Version` will be connected
// to application version and will be printed in all logs.
// TODO: Refactor after moving version.Info() and version.BuildContext() to https://github.com/percona/exporter_shared
// See: https://jira.percona.com/browse/PMM-3250 and https://github.com/percona/mongodb_exporter/pull/132#discussion_r262227248
func initVersionInfo() {
	version.Version = pmmVersion.Version
	version.Revision = pmmVersion.FullCommit
	version.Branch = pmmVersion.Branch

	if buildDate, err := strconv.ParseInt(pmmVersion.Timestamp, 10, 64); err != nil {
		version.BuildDate = time.Unix(0, 0).Format(versionDataFormat)
	} else {
		version.BuildDate = time.Unix(buildDate, 0).Format(versionDataFormat)
	}

	if pmmVersion.PMMVersion != "" {
		version.Version += "-pmm-" + pmmVersion.PMMVersion
		kingpin.Version(pmmVersion.FullInfo())
	} else {
		kingpin.Version(version.Print(program))
	}

	kingpin.HelpFlag.Short('h')
	kingpin.CommandLine.Help = fmt.Sprintf("%s exports various MongoDB metrics in Prometheus format.\n", pmmVersion.ShortInfo())
}

func newHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hostPort := r.URL.Query().Get("address")
		gatherKey := "default"
		if hostPort != "" {
			gatherKey = hostPort
		}

		gather, ok := gathersCache.Exists(gatherKey)
		if !ok {
			useAuth, err := strconv.ParseBool(r.URL.Query().Get("auth"))
			if err != nil {
				log.Debugln("query key parse bool fail:", "auth", r.URL.Query().Get("auth"))
			}

			if useAuth {
				log.Debugln("cache registry with credentials using configuration", gatherKey)
				gather, err = gathersCache.Add(gatherKey, hostPort, defaultConnUser, defaultConnPassword)
			} else {
				log.Debugln("cache registry no credentials", gatherKey)
				gather, err = gathersCache.Add(gatherKey, hostPort, "", "")
			}
			if err != nil {
				log.Errorln("can't create registry", err)
			}
		}

		log.Debugln("collector data", gatherKey)
		gathersCache.Gather(gather, w, r)
		return
	}

}

func replaceUri(uri, host, user, password string) (string, error) {
	p, err := url.Parse(uri)
	if err != nil {
		return uri, err
	}
	if host != "" {
		p.Host = host
	}
	if user != "" && password != "" {
		p.User = url.UserPassword(defaultConnUser, defaultConnPassword)
	}

	return p.String(), nil
}

func RedactMongoUri(uri string) string {
	if strings.HasPrefix(uri, "mongodb://") && strings.Contains(uri, "@") {
		if strings.Contains(uri, "ssl=true") {
			uri = strings.Replace(uri, "ssl=true", "", 1)
		}
		dialInfo, err := mgo.ParseURL(uri)
		if err != nil {
			log.Errorf("Cannot parse mongodb server url: %s", err)
			return "unknown/error"
		}
		if dialInfo.Username != "" && dialInfo.Password != "" {
			return "mongodb://****:****@" + strings.Join(dialInfo.Addrs, ",")
		}
	}
	return uri
}

func parseMongodbCnf(config interface{}) error {
	opts := ini.LoadOptions{
		// Mongodb ini file can have boolean keys.
		AllowBooleanKeys: true,
	}
	cfg, err := ini.LoadSources(opts, config)
	if err != nil {
		return fmt.Errorf("failed reading ini file: %s", err)
	}
	user := cfg.Section("client").Key("user").String()
	password := cfg.Section("client").Key("password").String()
	if (user == "") || (password == "") {
		return fmt.Errorf("no user or password specified under [client] in %s", config)
	}

	defaultConnUser = user
	defaultConnPassword = password
	return nil
}

type GatherersCache struct {
	cache   map[string]*prometheus.Gatherers
	expired map[string]int
}

func (s *GatherersCache) Exists(hostPort string) (*prometheus.Gatherers, bool) {
	if item, ok := s.cache[hostPort]; ok {
		return item, true
	}
	return nil, false
}

func (s *GatherersCache) Gather(g *prometheus.Gatherers, w http.ResponseWriter, r *http.Request) {
	p := promhttp.HandlerFor(g, promhttp.HandlerOpts{})
	p.ServeHTTP(w, r)

}

func (s *GatherersCache) Add(name, hostPort, user, password string) (*prometheus.Gatherers, error) {
	if g, ok := s.Exists(hostPort); ok {
		return g, errors.New("mongo gatherer exists")
	}
	uri := *uriF
	uri, err := replaceUri(uri, hostPort, user, password)
	if err != nil {
		return nil, err
	}
	log.Debugln("uri replaced to ", RedactMongoUri(uri))
	registry := prometheus.NewRegistry()
	log.Debugln("init registry")
	gatherers := prometheus.Gatherers{
		prometheus.DefaultGatherer,
		registry,
	}

	opts := &collector.MongodbCollectorOpts{
		URI:                      uri,
		TLSConnection:            *tlsF,
		TLSCertificateFile:       *tlsCertF,
		TLSPrivateKeyFile:        *tlsPrivateKeyF,
		TLSCaFile:                *tlsCAF,
		TLSHostnameValidation:    !(*tlsDisableHostnameValidationF),
		DBPoolLimit:              *maxConnectionsF,
		CollectDatabaseMetrics:   *collectDatabaseF,
		CollectCollectionMetrics: *collectCollectionF,
		CollectTopMetrics:        *collectTopF,
		CollectIndexUsageStats:   *collectIndexUsageF,
		SocketTimeout:            *socketTimeoutF,
		SyncTimeout:              *syncTimeoutF,
		AuthentificationDB:       *authDB,
	}

	mongodbCollector := collector.NewMongodbCollector(opts)
	log.Debugln("register collector", hostPort)
	registry.MustRegister(mongodbCollector)
	s.cache[name] = &gatherers
	return &gatherers, nil
}

func newMongoGatherersCache() *GatherersCache {
	return &GatherersCache{
		cache:   make(map[string]*prometheus.Gatherers),
		expired: make(map[string]int),
	}
}
