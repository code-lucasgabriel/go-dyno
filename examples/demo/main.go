// Command demo is a small HTTP service with three planted performance problems,
// one per endpoint, for trying go-dyno end to end:
//
//	GET /hot    a hotspot: compiles a regular expression on every request
//	GET /leak   a leak: every request registers a subscriber that is never removed
//	GET /work   a ceiling: a fixed pool of 4 workers, each holding a request for 20 ms
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/code-lucasgabriel/go-dyno/dyno"
)

func main() {
	stop, err := dyno.Start() // serves pprof + stats on DYNO_ADDR when set
	if err != nil {
		log.Fatal(err)
	}
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /hot", hot)
	mux.HandleFunc("GET /leak", leak)
	mux.HandleFunc("GET /work", work)

	for i := 0; i < workers; i++ {
		go worker()
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("demo listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, dyno.Middleware(mux)))
}

// ---- hotspot: the pattern never changes, but it is compiled on every call.

var (
	words   = strings.Fields("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu")
	pattern = func() string { // a blocklist of 200 terms, as a config would hold
		terms := make([]string, 200)
		for i := range terms {
			terms[i] = fmt.Sprintf("term%03d[a-z]*", i)
		}
		return `^(` + strings.Join(terms, "|") + `)$`
	}()
)

func hot(w http.ResponseWriter, r *http.Request) {
	re := regexp.MustCompile(pattern) // planted: compiled on every request; hoist it to a package variable
	n := 0
	for i := 0; i < 50; i++ {
		if re.MatchString(words[i%len(words)]) {
			n++
		}
	}
	json.NewEncoder(w).Encode(map[string]int{"matches": n})
}

// ---- leak: subscribers are added and never removed; each holds a goroutine and 16 KiB.

type subscriber struct {
	ch  chan []byte
	buf []byte
}

var (
	subsMu sync.Mutex
	subs   []*subscriber
)

func leak(w http.ResponseWriter, r *http.Request) {
	s := &subscriber{ch: make(chan []byte), buf: make([]byte, 16<<10)}
	subsMu.Lock()
	subs = append(subs, s) // planted: no unsubscribe when the request ends
	subsMu.Unlock()
	go func() {
		for m := range s.ch { // blocks forever: nobody sends or closes
			_ = m
		}
	}()
	fmt.Fprintf(w, "subscribed\n")
}

// ---- ceiling: every request waits for one of 4 workers that each spend 20 ms (a slow dependency).

const workers = 4

type job struct{ done chan struct{} }

var jobs = make(chan job)

func worker() {
	for j := range jobs {
		time.Sleep(20 * time.Millisecond) // planted: pool too small for the load
		close(j.done)
	}
}

func work(w http.ResponseWriter, r *http.Request) {
	j := job{done: make(chan struct{})}
	select {
	case jobs <- j:
	case <-r.Context().Done():
		return
	}
	<-j.done
	w.Write([]byte("done\n"))
}
