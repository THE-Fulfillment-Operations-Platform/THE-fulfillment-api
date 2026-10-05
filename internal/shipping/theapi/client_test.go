package theapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shapes below are what THE's code actually sends (read from its source), not
// its swagger: service_code an int in one place, hs_code a number, messages a
// bare string, cancel ids ints, success key misspelt "sucess".

func server(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok")
}

func TestCreatePackage_TolerantDecode(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/packages" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"package":{"id":46281,"code":"","status":"pending","service_code":3,"hs_code":39264000901,
			"total_cost":"4.89","weight":69,"zipcode":"94612"}}`)
	})
	p, err := c.CreatePackage(context.Background(), CreatePackageRequest{OrderNumber: "IL69"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "46281" || p.Status != "pending" || float64(p.TotalCost) != 4.89 || p.ServiceCode != "3" || float64(p.Weight) != 69 {
		t.Fatalf("decoded %+v", p)
	}
}

func TestErrors_MessagesArrayOrString(t *testing.T) {
	cases := map[string]string{
		`{"error":"Please check input data","messages":["The weight is required","The city is required"]}`: "The weight is required; The city is required",
		`{"error":"Please check input data","messages":"Missing package id"}`:                              "Missing package id",
		`{"error":"Not found"}`: "Not found",
	}
	for body, want := range cases {
		c := server(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, body)
		})
		_, err := c.CreatePackage(context.Background(), CreatePackageRequest{})
		var te *Error
		if !errors.As(err, &te) || te.Detail() != want {
			t.Errorf("body %s: got %v, want detail %q", body, err, want)
		}
	}
}

// 5xx (THE's panic page, a gateway error) says nothing about whether the
// request was applied: it must surface as "unknown", never as a refusal.
func TestServerErrorsAreUnknownOutcome(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "PANIC: runtime error: invalid memory address")
	})
	_, err := c.DeliverPackage(context.Background(), "12")
	if !IsTransport(err) {
		t.Fatalf("5xx must be an unknown outcome, got %v", err)
	}
}

func TestCancel_SendsIntegerIDs(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string][]json.Number
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("ids must be JSON numbers: %v", err)
		}
		if r.Method != http.MethodPut || len(body["ids"]) != 2 || body["ids"][0] != "113084" {
			t.Errorf("unexpected cancel %s %v", r.Method, body)
		}
		io.WriteString(w, `{"sucess":true}`)
	})
	if err := c.CancelPackages(context.Background(), []string{"113084", "113083"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelPackages(context.Background(), []string{"abc"}); err == nil {
		t.Fatal("a non-numeric id must be refused before the call")
	}
}

func TestDeliveryLabelFormats(t *testing.T) {
	pdf := []byte("%PDF-1.4 fake")
	cases := []struct {
		b64  string
		want string
	}{
		{base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nxx")), "image/png"},
		{base64.StdEncoding.EncodeToString(pdf), "application/pdf"},
		{"data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf), "application/pdf"},
		{"https://label.example/x.pdf", ""}, // a URL: fetch by tracking instead
		{"", ""},
		{base64.StdEncoding.EncodeToString([]byte("<html>")), ""},
	}
	for _, c := range cases {
		d := DeliverResult{Base64Label: c.b64}
		_, got := d.Label()
		if got != c.want {
			t.Errorf("label %q: type %q want %q", c.b64[:min(len(c.b64), 20)], got, c.want)
		}
	}
}

func TestGetPackage_LastMile(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/packages/43354") {
			t.Errorf("path %s", r.URL.Path)
		}
		io.WriteString(w, `{"package":{"id":43354,"code":"THE0000512000433544","status":"pre-transit","hs_code":201,
			"tracking":{"tracking_number":"9200100000000000841168","last_mile_carrier":"USPS","label_url":"x"}}}`)
	})
	d, err := c.GetPackage(context.Background(), "43354")
	if err != nil {
		t.Fatal(err)
	}
	n, carrier := d.LastMile()
	if n != "9200100000000000841168" || carrier != "USPS" || d.Code != "THE0000512000433544" {
		t.Fatalf("last mile %q %q code %q", n, carrier, d.Code)
	}
}
