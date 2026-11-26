package governor

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestM3NextTurnWithRegularWaiters(t *testing.T) {
	g := setup(t)
	paths := []string{
		"/repos/chriscoveriesorg/wick/actions/runs?status=queued",
		"/repos/chriscoveriesorg/seatkit/commits",
		"/repos/chriscoveriesorg/hostops/pulls/99",
		"/repos/chriscoveriesorg/conquest-reborn/actions/runs?status=in_progress",
		"/repos/chriscoveriesorg/conquest-reborn/actions/runs?status=queued",
	}
	replies := fixture(t)
	dispatched := make(chan string, len(paths))
	advance := make(chan struct{})
	var inFlight atomic.Int32
	g.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if inFlight.Add(1) != 1 {
			t.Error("concurrent upstream dispatch")
		}
		defer inFlight.Add(-1)
		dispatched <- r.URL.RequestURI()
		<-advance
		return response(http.StatusOK, replies, nil), nil
	})

	var requests sync.WaitGroup
	t.Cleanup(func() {
		close(advance)
		requests.Wait()
	})
	start := func(index int, lane string) {
		requests.Add(1)
		go func() {
			defer requests.Done()
			result := g.RoundTrip(req(http.MethodGet, paths[index]), Caller{Class: "owner", Lane: lane})
			defer result.Body.Close()
			if result.StatusCode != http.StatusOK {
				t.Errorf("%s returned %d", paths[index], result.StatusCode)
			}
		}()
	}
	wantDispatch := func(index int) {
		t.Helper()
		select {
		case got := <-dispatched:
			if got != paths[index] {
				t.Fatalf("dispatched %q, want %q", got, paths[index])
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %s", paths[index])
		}
	}
	waitQueued := func(regular, m3 int) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			g.admission.mu.Lock()
			matches := len(g.admission.regular) == regular && len(g.admission.m3) == m3
			g.admission.mu.Unlock()
			if matches {
				return
			}
			select {
			case <-ticker.C:
			case <-deadline:
				t.Fatalf("timed out waiting for %d regular and %d M3 waiters", regular, m3)
			}
		}
	}

	start(0, "wickdev")
	wantDispatch(0)
	start(1, "cifast-devin-rt")
	waitQueued(1, 0)
	start(2, "cifast-devin-rt")
	waitQueued(2, 0)
	start(3, "cifast-m3")
	waitQueued(2, 1)
	start(4, "cifast-m3")
	waitQueued(2, 2)

	// Each M3 request gets the next available priority turn, and a regular
	// request goes immediately after it even while another M3 is waiting.
	for _, index := range []int{3, 1, 4, 2} {
		advance <- struct{}{}
		wantDispatch(index)
	}
	advance <- struct{}{}
	requests.Wait()
}
