// This is a test server that implements the subset of RFC6962 APIs needed to
// run Boulder's CT log submission code. Currently it only implements add-chain.
// This is used by startservers.py.
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/publisher"
)

type ctSubmissionRequest struct {
	Chain []string `json:"chain"`
}

type integrationSrv struct {
	sync.Mutex
	submissions map[string]int64
	// Hostnames where we refuse to provide an SCT. This is to exercise the code
	// path where all CT servers fail.
	rejectHosts map[string]bool
	// A list of entries that we rejected based on rejectHosts.
	rejected        []string
	key             *ecdsa.PrivateKey
	latencySchedule []float64
	latencyItem     int
}

func readJSON(w http.ResponseWriter, r *http.Request, output interface{}) error {
	if r.Method != "POST" {
		return fmt.Errorf("incorrect method; only POST allowed")
	}
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(bodyBytes, output)
	if err != nil {
		return err
	}
	return nil
}

func (is *integrationSrv) addChain(w http.ResponseWriter, r *http.Request) {
	is.addChainOrPre(w, r, false)
}

// addRejectHost takes a JSON POST with a "host" field; any subsequent
// submissions for that host will get a 400 error.
func (is *integrationSrv) addRejectHost(w http.ResponseWriter, r *http.Request) {
	var rejectHostReq struct {
		Host string
	}
	err := readJSON(w, r, &rejectHostReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	is.Lock()
	defer is.Unlock()
	is.rejectHosts[rejectHostReq.Host] = true
	w.Write([]byte{})
}

// getRejections returns a JSON array containing strings; those strings are
// base64 encodings of certificates or precertificates that were rejected due to
// the rejectHosts mechanism.
func (is *integrationSrv) getRejections(w http.ResponseWriter, r *http.Request) {
	is.Lock()
	defer is.Unlock()
	output, err := json.Marshal(is.rejected)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(output)
}

func (is *integrationSrv) addPreChain(w http.ResponseWriter, r *http.Request) {
	is.addChainOrPre(w, r, true)
}

func (is *integrationSrv) addChainOrPre(w http.ResponseWriter, r *http.Request, precert bool) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	bodyBytes, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var addChainReq ctSubmissionRequest
	err = json.Unmarshal(bodyBytes, &addChainReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(addChainReq.Chain) == 0 {
		w.WriteHeader(400)
		return
	}

	b, err := base64.StdEncoding.DecodeString(addChainReq.Chain[0])
	if err != nil {
		w.WriteHeader(400)
		return
	}
	cert, err := x509.ParseCertificate(b)
	if err != nil {
		w.WriteHeader(400)
		return
	}
	hostnames := strings.Join(cert.DNSNames, ",")

	is.Lock()
	for _, h := range cert.DNSNames {
		if is.rejectHosts[h] {
			is.Unlock()
			is.rejected = append(is.rejected, addChainReq.Chain[0])
			w.WriteHeader(400)
			return
		}
	}

	is.submissions[hostnames]++
	is.Unlock()

	if is.latencySchedule != nil {
		is.Lock()
		sleepTime := time.Duration(is.latencySchedule[is.latencyItem%len(is.latencySchedule)]) * time.Second
		is.latencyItem++
		is.Unlock()
		time.Sleep(sleepTime)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(publisher.CreateTestingSignedSCT(addChainReq.Chain, is.key, precert, time.Now()))
}

func (is *integrationSrv) getSubmissions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}

	is.Lock()
	hostnames := r.URL.Query().Get("hostnames")
	submissions := is.submissions[hostnames]
	is.Unlock()

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "%d", submissions)
}

type config struct {
	Personalities []Personality
}

type Personality struct {
	// Port (and optionally IP) to listen on
	Addr string
	// Private key for signing SCTs
	// Generate your own with:
	// openssl ecparam -name prime256v1 -genkey -outform der -noout | base64 -w 0
	PrivKey string
	// If present, sleep for the given number of seconds before replying. Each
	// request uses the next number in the list, eventually cycling through.
	LatencySchedule []float64
}

func runPersonality(p Personality) {
	keyDER, err := base64.StdEncoding.DecodeString(p.PrivKey)
	if err != nil {
		log.Fatal(err)
	}
	key, err := x509.ParseECPrivateKey(keyDER)
	if err != nil {
		log.Fatal(err)
	}
	pubKeyBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		log.Fatal(err)
	}
	is := integrationSrv{
		key:             key,
		latencySchedule: p.LatencySchedule,
		submissions:     make(map[string]int64),
		rejectHosts:     make(map[string]bool),
	}
	m := http.NewServeMux()
	m.HandleFunc("/submissions", is.getSubmissions)
	m.HandleFunc("/ct/v1/add-pre-chain", is.addPreChain)
	m.HandleFunc("/ct/v1/add-chain", is.addChain)
	m.HandleFunc("/add-reject-host", is.addRejectHost)
	m.HandleFunc("/get-rejections", is.getRejections)
	srv := &http.Server{
		Addr:    p.Addr,
		Handler: m,
	}
	log.Printf("ct-test-srv on %s with pubkey %s", p.Addr,
		base64.StdEncoding.EncodeToString(pubKeyBytes))
	log.Fatal(srv.ListenAndServe())
}

func main() {
	configFile := flag.String("config", "", "Path to config file.")
	flag.Parse()
	data, err := ioutil.ReadFile(*configFile)
	if err != nil {
		log.Fatal(err)
	}
	var c config
	err = json.Unmarshal(data, &c)
	if err != nil {
		log.Fatal(err)
	}

	for _, p := range c.Personalities {
		go runPersonality(p)
	}
	cmd.CatchSignals(nil, nil)
}
