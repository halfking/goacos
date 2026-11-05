package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The application model is goacos's own surface, not a Nacos one.
//
// These tests pin two things that are easy to get wrong and hard to notice:
// the writes go to /goacos/v1 (NOT /nacos), and they happen in the order the
// dependencies require — a route points at a component, and a component
// belongs to an application.

type recordedWrite struct {
	method string
	path   string
	body   map[string]any
}

// recordingServer answers every write with an OK envelope and remembers what
// it was asked to write.
func recordingServer(t *testing.T, recorded *[]recordedWrite, failPath string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		*recorded = append(*recorded, recordedWrite{method: r.Method, path: r.URL.Path, body: body})
		if failPath != "" && strings.Contains(r.URL.Path, failPath) {
			w.WriteHeader(http.StatusBadRequest)
			writeEnvelope(t, w, "refused on purpose")
			return
		}
		writeEnvelope(t, w, "ok")
	}))
	return srv
}

// The whole point of this file: a service can declare what it IS.
func TestPublishApplicationModelWritesInDependencyOrder(t *testing.T) {
	var recorded []recordedWrite
	srv := recordingServer(t, &recorded, "")
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())

	err := c.PublishApplicationModel(context.Background(),
		Application{
			NamespaceID: "public",
			Name:        "acc",
			DisplayName: "Agent Control Center",
			Env:         "itestu",
		},
		[]Component{
			{Name: "api", Kind: "api", Protocol: "http", ServiceName: "acc-go", Weight: 1},
			{Name: "web", Kind: "web", Protocol: "http", ServiceName: "acc-nginx", Weight: 1},
		},
		[]Route{
			{Domain: "acc.itestu.cn", PathPrefix: "/", ComponentName: "api"},
		})
	if err != nil {
		t.Fatalf("PublishApplicationModel: %v", err)
	}

	if len(recorded) != 4 {
		t.Fatalf("expected 4 writes (1 app + 2 components + 1 route), got %d: %+v",
			len(recorded), recorded)
	}

	// The order is the dependency order.
	if recorded[0].path != goacosPrefix+"/apps" || recorded[0].method != http.MethodPost {
		t.Errorf("first write must be the application, got %s %s", recorded[0].method, recorded[0].path)
	}
	if recorded[1].path != goacosPrefix+"/apps/acc/components/api" {
		t.Errorf("components must be written under their app, got %s", recorded[1].path)
	}
	if recorded[2].path != goacosPrefix+"/apps/acc/components/web" {
		t.Errorf("components must be written under their app, got %s", recorded[2].path)
	}
	if recorded[3].path != goacosPrefix+"/routes" || recorded[3].method != http.MethodPost {
		t.Errorf("routes must be written last, got %s %s", recorded[3].method, recorded[3].path)
	}

	// The app model does NOT live under the Nacos prefix. A write that landed
	// there would 404, and the failure would look like "the server has no
	// application model" rather than "the client used the wrong prefix".
	for _, w := range recorded {
		if strings.HasPrefix(w.path, pathPrefix) {
			t.Errorf("application model writes must not go to the naming prefix: %s", w.path)
		}
	}

	// A component's identity is in the URL, and the component inherits the
	// application's namespace when it did not name one.
	if got := recorded[1].body["namespaceId"]; got != "public" {
		t.Errorf("component namespace = %v, want public (inherited from the application)", got)
	}
	if got := recorded[1].body["serviceName"]; got != "acc-go" {
		t.Errorf("component serviceName = %v, want acc-go", got)
	}
	if got := recorded[3].body["namespaceId"]; got != "public" {
		t.Errorf("route namespace = %v, want public (inherited from the application)", got)
	}
	if got := recorded[3].body["appName"]; got != "acc" {
		t.Errorf("route appName = %v, want acc (inherited from the application)", got)
	}
}

// A route with no component would leave the gateway with nothing to forward
// to. Refuse it before it reaches the wire.
func TestPublishRouteRefusesAnIncompleteDeclaration(t *testing.T) {
	var recorded []recordedWrite
	srv := recordingServer(t, &recorded, "")
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())

	cases := []struct {
		name  string
		route Route
	}{
		{"no domain", Route{AppName: "acc", ComponentName: "api"}},
		{"no app", Route{Domain: "acc.itestu.cn", ComponentName: "api"}},
		{"no component", Route{Domain: "acc.itestu.cn", AppName: "acc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.PublishRoute(context.Background(), tc.route); err == nil {
				t.Fatalf("an incomplete route must be refused")
			}
		})
	}
	if len(recorded) != 0 {
		t.Errorf("a refused declaration must not reach the wire: %+v", recorded)
	}
}

// An application with no name has no identity in the routing table.
func TestPublishApplicationRefusesAnEmptyName(t *testing.T) {
	var recorded []recordedWrite
	srv := recordingServer(t, &recorded, "")
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())

	if err := c.PublishApplication(context.Background(), Application{DisplayName: "x"}); err == nil {
		t.Fatal("an application with no name must be refused")
	}
	if len(recorded) != 0 {
		t.Errorf("a refused declaration must not reach the wire: %+v", recorded)
	}
}

// A half-published model is a real state that somebody has to know about.
//
// The gateway would route part of a domain to a component that has no
// declaration behind it, and nothing in the status output would say so.
func TestPublishApplicationModelStopsAtTheFirstFailureAndNamesTheStep(t *testing.T) {
	var recorded []recordedWrite
	srv := recordingServer(t, &recorded, "/components/broken")
	defer srv.Close()

	c := NewClient(testConfig(srv), testLogger())

	err := c.PublishApplicationModel(context.Background(),
		Application{Name: "acc"},
		[]Component{
			{Name: "api", Kind: "api", Protocol: "http", ServiceName: "acc-go"},
			{Name: "broken", Kind: "api", Protocol: "http", ServiceName: "nope"},
		},
		[]Route{{Domain: "acc.itestu.cn", ComponentName: "api"}})
	if err == nil {
		t.Fatal("a failing write must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("the error must name the step that failed, got %v", err)
	}
	// The route must NOT have been written: the model stopped at the broken
	// component.
	if len(recorded) != 3 {
		t.Errorf("expected the app and two components, and nothing after the failure, got %d: %+v",
			len(recorded), recorded)
	}
}
