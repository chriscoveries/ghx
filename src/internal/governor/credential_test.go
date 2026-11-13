package governor

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Only retained bytes are response data. Header/status substitutions below
// are named fault controls; none is presented as a captured upstream result.
func capturedResponse(t *testing.T) *http.Response {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/api-throttle/issue140.http")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(raw))), nil)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func observedPoison(t *testing.T) Rate {
	t.Helper()
	raw, err := os.ReadFile("../../../testdata/owner-core-poison-20261113/observed-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		OwnerCore struct {
			Remaining int
			Reset     string
		} `json:"owner_core"`
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		t.Fatal(err)
	}
	reset, err := time.Parse(time.RFC3339, observed.OwnerCore.Reset)
	if err != nil {
		t.Fatal(err)
	}
	return Rate{Remaining: observed.OwnerCore.Remaining, Reset: reset.Unix()}
}

func ownFingerprint(t *testing.T) string {
	t.Helper()
	value, err := credentialFingerprint(req("GET", "/").Header)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAnonymousResponseCannotPoisonOwnerRate(t *testing.T) {
	for _, path := range []string{"/repos/o/r/issues/140", "/rate_limit", "/graphql"} {
		t.Run(path, func(t *testing.T) {
			g := setup(t)
			before := observedPoison(t)
			// Healthy owner quota is the other actual observation in the
			// incident. Seed an unbound legacy slot so the floor itself cannot
			// accidentally make a missing-auth regression pass.
			before.Remaining = 4411
			g.Now = func() time.Time { return time.Unix(before.Reset-100, 0) }
			g.State.Rates["owner/core"] = before
			calls := 0
			g.Transport = transport(func(request *http.Request) (*http.Response, error) {
				calls++
				wire := capturedResponse(t)
				// Observed anonymous59/reset injected into retained response.
				wire.Header.Set("X-RateLimit-Remaining", "59")
				wire.Header.Set("X-RateLimit-Reset", strconv.FormatInt(before.Reset, 10))
				return wire, nil
			})
			request := req("GET", path)
			request.Header.Del("Authorization")
			result := g.RoundTrip(request, caller)
			if result.StatusCode != 403 || calls != 0 {
				t.Fatal("anonymous request reached transport")
			}
			content(t, result)
			if g.State.Rates["owner/core"] != before {
				t.Fatal("anonymous response changed owner/core")
			}
		})
	}
}

func TestCredentialParsingIsCanonicalAndRejectsAmbiguity(t *testing.T) {
	original := req("GET", "/")
	first, err := credentialFingerprint(original.Header)
	if err != nil {
		t.Fatal(err)
	}
	original.Header.Set("Authorization", "token fixture-owner")
	second, err := credentialFingerprint(original.Header)
	if err != nil || first != second {
		t.Fatal("same credential split by auth scheme")
	}
	for _, values := range [][]string{nil, {""}, {"Bearer "}, {"Basic fixture-owner"}, {"Bearer one", "Bearer two"}} {
		header := http.Header{}
		for _, value := range values {
			header.Add("Authorization", value)
		}
		if _, err := credentialFingerprint(header); err == nil {
			t.Fatal("ambiguous credential accepted")
		}
	}
}

func TestConfiguredCredentialOverridesIncomingFingerprint(t *testing.T) {
	g := setup(t)
	g.Policy.Tokens["owner"] = "GHGATE_CREDENTIAL_TEST"
	t.Setenv("GHGATE_CREDENTIAL_TEST", "configured-fixture-owner")
	g.Transport = transport(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer configured-fixture-owner" {
			t.Fatal("override not forwarded")
		}
		return capturedResponse(t), nil
	})
	content(t, g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller))
	header := http.Header{"Authorization": {"Bearer configured-fixture-owner"}}
	want, err := credentialFingerprint(header)
	if err != nil || g.State.Rates["owner/core"].CredentialSHA256 != want {
		t.Fatal("incoming credential acquired custody")
	}
}

func TestForeignCredentialCannotChangeActiveIdentityResource(t *testing.T) {
	for _, key := range []string{"owner/core", "owner/search", "owner/graphql"} {
		g := setup(t)
		before := Rate{Remaining: 1000, Reset: 11000, CredentialSHA256: ownFingerprint(t)}
		g.State.Rates[key] = before
		calls := 0
		g.Transport = transport(func(request *http.Request) (*http.Response, error) { calls++; return capturedResponse(t), nil })
		request := req("GET", "/repos/o/r/issues/140")
		request.Header.Set("Authorization", "Bearer rotated-fixture-owner")
		result := g.RoundTrip(request, caller)
		if result.StatusCode != 403 || calls != 0 || g.State.Rates[key] != before {
			t.Fatal("active credential binding changed")
		}
		content(t, result)
	}
}

func TestFreeDiagnosticNeverBindsOrChangesAnotherCredentialRate(t *testing.T) {
	g := setup(t)
	before := Rate{Remaining: 59, Reset: 11000, Blocked: 12000, Failures: 4, CredentialSHA256: ownFingerprint(t)}
	g.State.Rates["owner/core"] = before
	g.Transport = transport(func(request *http.Request) (*http.Response, error) { return capturedResponse(t), nil })
	request := req("GET", "/rate_limit")
	request.Header.Set("Authorization", "Bearer rotated-fixture-owner")
	result := g.RoundTrip(request, caller)
	if result.StatusCode != 200 || g.State.Rates["owner/core"] != before {
		t.Fatal("diagnostic altered binding or floor")
	}
	content(t, result)
}

func TestLegacyFloorSurvivesAdmittedWriteAndMeasurement(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		g := setup(t)
		before := observedPoison(t)
		g.Now = func() time.Time { return time.Unix(before.Reset-100, 0) }
		g.State.Rates["owner/core"] = before
		g.Transport = transport(func(request *http.Request) (*http.Response, error) { return capturedResponse(t), nil })
		path := "/repos/o/r/issues/140"
		if method == "GET" {
			path = "/rate_limit"
		}
		content(t, g.RoundTrip(req(method, path), caller))
		after := g.State.Rates["owner/core"]
		if after.CredentialSHA256 != "" || after.Remaining > before.Remaining || after.Reset < before.Reset {
			t.Fatal("legacy floor relabelled or relaxed")
		}
	}
}

func TestCredentialRebindWaitsForBothDeadlines(t *testing.T) {
	g := setup(t)
	old := Rate{Remaining: 59, Reset: 10001, Blocked: 10002, Failures: 4, CredentialSHA256: ownFingerprint(t)}
	g.State.Rates["owner/core"] = old
	g.Transport = transport(func(request *http.Request) (*http.Response, error) { return capturedResponse(t), nil })
	request := req("GET", "/repos/o/r/issues/140")
	request.Header.Set("Authorization", "Bearer rotated-fixture-owner")
	g.Now = func() time.Time { return time.Unix(10001, 0) }
	result := g.RoundTrip(request, caller)
	if result.StatusCode != 403 || g.State.Rates["owner/core"] != old {
		t.Fatal("rebound before cooldown expired")
	}
	content(t, result)
	g.Now = func() time.Time { return time.Unix(10002, 0) }
	content(t, g.RoundTrip(request, caller))
	want, err := credentialFingerprint(request.Header)
	if err != nil || g.State.Rates["owner/core"].CredentialSHA256 != want || g.State.Rates["owner/core"].Failures != 0 {
		t.Fatal("exact-expiry rebind failed")
	}
	if len(g.State.Rates) != 1 {
		t.Fatal("token rotation grew rate map")
	}
}

func TestUnauthorizedResponseCannotBindOrReplaceRate(t *testing.T) {
	for _, before := range []Rate{{}, {Remaining: 1000, Reset: 11000, CredentialSHA256: ownFingerprint(t)}} {
		g := setup(t)
		g.State.Rates["owner/core"] = before
		g.Transport = transport(func(request *http.Request) (*http.Response, error) {
			wire := capturedResponse(t)
			wire.StatusCode = 401 // named invalid-auth response control
			wire.Header.Set("X-RateLimit-Remaining", "59")
			return wire, nil
		})
		result := g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller)
		if result.StatusCode != 401 || g.State.Rates["owner/core"] != before {
			t.Fatal("invalid bearer changed rate custody")
		}
		content(t, result)
	}
}

func TestObservedResourceCollisionCannotOverwriteForeignBinding(t *testing.T) {
	g := setup(t)
	request := req("GET", "/search/code?q=fixture")
	foreign := req("GET", "/")
	foreign.Header.Set("Authorization", "Bearer rotated-fixture-owner")
	fingerprint, err := credentialFingerprint(foreign.Header)
	if err != nil {
		t.Fatal(err)
	}
	before := Rate{Remaining: 1000, Reset: 11000, CredentialSHA256: fingerprint}
	g.Transport = transport(func(request *http.Request) (*http.Response, error) {
		// Explicit out-of-band state fault after the preflight scan. Ordinary
		// requests are serialized; this exercises the independent destination guard.
		g.State.Rates["owner/core"] = before
		wire := capturedResponse(t)
		wire.Header.Set("X-RateLimit-Resource", "core")
		return wire, nil
	})
	result := g.RoundTrip(request, caller)
	if result.StatusCode != 403 || g.State.Rates["owner/core"] != before {
		t.Fatal("observed-key collision overwrote custody")
	}
	content(t, result)
}

func TestFingerprintPersistsAndMalformedDigestRefusesRestart(t *testing.T) {
	g := setup(t)
	g.Transport = transport(func(request *http.Request) (*http.Response, error) { return capturedResponse(t), nil })
	content(t, g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller))
	reloaded, err := New(g.Dir)
	if err != nil || !reflect.DeepEqual(g.State.Rates, reloaded.State.Rates) {
		t.Fatal("restart lost credential custody")
	}
	bad := g.State.Rates["owner/core"]
	bad.CredentialSHA256 = "malformed"
	g.State.Rates["owner/core"] = bad
	if err := save(filepath.Join(g.Dir, "state.json"), g.State); err != nil {
		t.Fatal(err)
	}
	if _, err := New(g.Dir); err == nil {
		t.Fatal("malformed persisted digest accepted")
	}
}

func TestConfiguredCredentialMissingRefusesRatherThanUsingIncomingBearer(t *testing.T) {
	g := setup(t)
	g.Policy.Tokens["owner"] = "GHGATE_CREDENTIAL_MISSING_TEST"
	t.Setenv("GHGATE_CREDENTIAL_MISSING_TEST", "")
	calls := 0
	g.Transport = transport(func(request *http.Request) (*http.Response, error) { calls++; return capturedResponse(t), nil })
	result := g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller)
	if result.StatusCode != 403 || calls != 0 {
		t.Fatal("missing configured token fell back to incoming token")
	}
	content(t, result)
}

func TestSameCredentialRetainsItsRecordedFloor(t *testing.T) {
	g := setup(t)
	before := Rate{Remaining: 59, Reset: 11000, CredentialSHA256: ownFingerprint(t)}
	g.State.Rates["owner/core"] = before
	calls := 0
	g.Transport = transport(func(request *http.Request) (*http.Response, error) { calls++; return capturedResponse(t), nil })
	result := g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller)
	if result.StatusCode != 403 || calls != 0 || g.State.Rates["owner/core"] != before {
		t.Fatal("same-token floor changed")
	}
	content(t, result)
}

func TestHealthyWriteCannotRenewLegacyAnonymousEpoch(t *testing.T) {
	g := setup(t)
	before := observedPoison(t)
	g.Now = func() time.Time { return time.Unix(before.Reset-100, 0) }
	g.State.Rates["owner/core"] = before
	g.Transport = transport(func(request *http.Request) (*http.Response, error) {
		wire := capturedResponse(t)
		// Named reset mutation: a healthy token has a later quota epoch.
		wire.Header.Set("X-RateLimit-Reset", strconv.FormatInt(before.Reset+3600, 10))
		return wire, nil
	})
	content(t, g.RoundTrip(req("POST", "/repos/o/r/issues/140"), caller))
	if g.State.Rates["owner/core"] != before {
		t.Fatal("healthy later epoch renewed old anonymous floor")
	}
	g.Now = func() time.Time { return time.Unix(before.Reset, 0) }
	content(t, g.RoundTrip(req("GET", "/repos/o/r/issues/140"), caller))
	if g.State.Rates["owner/core"].CredentialSHA256 != ownFingerprint(t) {
		t.Fatal("original legacy expiry did not bind current credential")
	}
}
