package services

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"

	"the-fulfillment/backend/internal/models"
)

// Importing outside labels ("Link label") through the tracking Excel flow, and
// printing them back out. Same preview → commit contract as the tracking number:
// nothing is written by the preview; an order that already holds a different
// link is only replaced on the operator's explicit opt-in (the client sends
// OVERWRITE lines only then), and every uncertain line becomes an issue.

const (
	lblA = "https://file.gkelogistics.com/labels/A.pdf"
	lblB = "https://file.gkelogistics.com/labels/B.pdf"
	lblC = "https://file.gkelogistics.com/labels/C.pdf"
)

var csActor = Actor{ID: 1, Role: models.RoleCS}

func seedLabelOrder(t *testing.T, svc *OrderService, code, store, label string) *models.Order {
	t.Helper()
	o := seedImportOrder(t, svc.repo.DB, code, store, "", nil)
	if label != "" {
		if err := svc.repo.DB.Model(o).Update("external_label_url", label).Error; err != nil {
			t.Fatalf("seed label: %v", err)
		}
	}
	return o
}

func TestExternalLabel_ParseColumns(t *testing.T) {
	for _, header := range []string{
		"Mã đơn,Link label", "OrderID,Label URL", "order_id,label", "Mã đơn,Mã vận đơn,Link Label",
	} {
		file, err := ParseTrackingCSVFile(strings.NewReader(header + "\n4141801137,https://x.test/a.pdf\n"))
		if err != nil {
			t.Fatalf("header %q rejected: %v", header, err)
		}
		r := file.Rows[0]
		if r.OrderKey != "4141801137" {
			t.Fatalf("header %q: %+v", header, r)
		}
		if r.LabelURL == "" && r.TrackingNumber == "" {
			t.Fatalf("header %q lost the value: %+v", header, r)
		}
	}
	// Label-only file: tracking column absent is fine.
	file, err := ParseTrackingCSVFile(strings.NewReader("Mã đơn,Link label\nA1," + lblA + "\n"))
	if err != nil || file.Rows[0].LabelURL != lblA || file.Rows[0].TrackingNumber != "" {
		t.Fatalf("label-only file: %+v %v", file, err)
	}
	// Neither a tracking nor a label column: still rejected.
	if _, err := ParseTrackingCSVFile(strings.NewReader("Mã đơn,Ghi chú\nA1,x\n")); err == nil {
		t.Fatal("a file with neither tracking nor label column must be rejected")
	}
}

func TestExternalLabel_NormalizeURL(t *testing.T) {
	good := []string{lblA, "http://host.test/a?x=1&y=2", "  " + lblA + "  "}
	for _, u := range good {
		if _, why := normalizeExternalLabelURL(u); why != "" {
			t.Errorf("%q should be valid, got %q", u, why)
		}
	}
	bad := []string{
		"ftp://host.test/a.pdf", "javascript:alert(1)", "file.gkelogistics.com/a.pdf", "https://",
		"https://host.test/a b.pdf", "https://user:pw@host.test/a.pdf", "//host.test/a.pdf",
		"https://host.test/" + strings.Repeat("a", MaxExternalLabelURLLen),
	}
	for _, u := range bad {
		if _, why := normalizeExternalLabelURL(u); why == "" {
			t.Errorf("%q should be rejected", u)
		}
	}
}

func TestExternalLabel_PreviewActions(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	a := seedLabelOrder(t, svc, "100001", "SO-A", "")   // new label
	b := seedLabelOrder(t, svc, "100002", "SO-B", lblB) // same link → unchanged
	c := seedLabelOrder(t, svc, "100003", "SO-C", lblA) // different link → overwrite
	seedLabelOrder(t, svc, "100004", "SO-D", "")        // matched by internal code below

	rows := []TrackingImportRow{
		{Row: 1, OrderKey: "SO-A", LabelURL: lblA},                    // by store order id
		{Row: 2, OrderKey: "SO-B", LabelURL: lblB},                    // unchanged
		{Row: 3, OrderKey: "SO-C", LabelURL: lblC},                    // overwrite lblA
		{Row: 4, OrderKey: "100004", LabelURL: "http://x.test/d.pdf"}, // by internal code
		{Row: 5, OrderKey: "NOPE", LabelURL: lblC},                    // unknown order
		{Row: 6, OrderKey: "SO-A", LabelURL: "ftp://bad"},             // bad link
	}
	p, err := svc.PreviewTrackingImport(csActor, rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	got := map[uint]TrackingImportMatch{}
	for _, m := range p.Matches {
		got[m.OrderID] = m
	}
	if got[a.ID].LabelAction != TrackingActionAssign || got[a.ID].Action != TrackingActionNone {
		t.Errorf("A: %+v", got[a.ID])
	}
	if got[b.ID].LabelAction != TrackingActionUnchanged {
		t.Errorf("B: %+v", got[b.ID])
	}
	if got[c.ID].LabelAction != TrackingActionOverwrite || got[c.ID].CurrentLabel != lblA {
		t.Errorf("C: %+v", got[c.ID])
	}
	if len(p.Matches) != 4 {
		t.Errorf("want 4 matches, got %d: %+v", len(p.Matches), p.Matches)
	}
	codes := map[string]int{}
	for _, is := range p.Issues {
		codes[is.Code]++
	}
	if codes[TrackingIssueNotFound] != 1 || codes[TrackingIssueBadLabel] != 1 {
		t.Errorf("issues: %v", codes)
	}
	s := p.Summary
	if s.LabelAssign != 2 || s.LabelUnchanged != 1 || s.LabelOverwrite != 1 || s.Assign != 0 || s.Overwrite != 0 {
		t.Errorf("summary: %+v", s)
	}
	// Preview writes nothing.
	var o models.Order
	svc.repo.DB.First(&o, a.ID)
	if o.ExternalLabelURL != "" {
		t.Errorf("preview wrote a label: %q", o.ExternalLabelURL)
	}
}

// One label shared by several product lines of one order (the JFT sheet): one
// assignment, no alarm; two different links for one order: dropped.
func TestExternalLabel_SeveralLinesOneOrder(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	a := seedLabelOrder(t, svc, "100001", "SO-A", "")
	b := seedLabelOrder(t, svc, "100002", "SO-B", "")
	c := seedLabelOrder(t, svc, "100003", "SO-C", "")

	rows := []TrackingImportRow{
		{Row: 1, OrderKey: "SO-A", LabelURL: lblA},
		{Row: 2, OrderKey: "SO-A", LabelURL: lblA},                         // same link on the next product line
		{Row: 3, OrderKey: "SO-A", TrackingNumber: "TN-A", LabelURL: lblA}, // a third line also carrying a number
		{Row: 4, OrderKey: "SO-B", LabelURL: lblB},
		{Row: 5, OrderKey: "SO-B", LabelURL: lblC}, // different link for the same order
		{Row: 6, OrderKey: "SO-C", TrackingNumber: "TN-C"},
		{Row: 7, OrderKey: "SO-C", LabelURL: lblC}, // number on one line, link on the next
	}
	p, err := svc.PreviewTrackingImport(csActor, rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	got := map[uint]TrackingImportMatch{}
	for _, m := range p.Matches {
		got[m.OrderID] = m
	}
	if len(p.Matches) != 2 {
		t.Fatalf("want matches for A and C only, got %+v", p.Matches)
	}
	if m := got[a.ID]; m.LabelURL != lblA || m.TrackingNumber != "TN-A" || m.LabelAction != TrackingActionAssign || m.Action != TrackingActionAssign {
		t.Errorf("A should merge link + number: %+v", m)
	}
	if m := got[c.ID]; m.LabelURL != lblC || m.TrackingNumber != "TN-C" {
		t.Errorf("C should merge number + link: %+v", m)
	}
	if _, ok := got[b.ID]; ok {
		t.Errorf("B has two different links, must be dropped: %+v", got[b.ID])
	}
	codes := map[string]int{}
	for _, is := range p.Issues {
		codes[is.Code]++
	}
	if codes[TrackingIssueConflict] != 2 || len(p.Issues) != 2 {
		t.Errorf("want exactly the 2 conflict lines of B, got %+v", p.Issues)
	}
	if p.Summary.MergedRows != 3 { // A: rows 2,3 ; C: row 7
		t.Errorf("merged rows: %+v", p.Summary)
	}
}

func TestExternalLabel_SameLinkOnTwoOrders(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	seedLabelOrder(t, svc, "100001", "SO-A", "")
	seedLabelOrder(t, svc, "100002", "SO-B", "")
	p, _ := svc.PreviewTrackingImport(csActor, []TrackingImportRow{
		{Row: 1, OrderKey: "SO-A", LabelURL: lblA}, {Row: 2, OrderKey: "SO-B", LabelURL: lblA},
	}, nil, nil)
	if len(p.Matches) != 0 || len(p.Issues) != 2 || p.Issues[0].Code != TrackingIssueSharedLabel {
		t.Fatalf("one link on two orders must be refused: %+v", p)
	}
}

func TestExternalLabel_CommitWritesLabelOnly(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	a := seedLabelOrder(t, svc, "100001", "SO-A", "")
	b := seedLabelOrder(t, svc, "100002", "SO-B", lblA)
	c := seedLabelOrder(t, svc, "100003", "SO-C", "")

	res, err := svc.CommitTrackingImport(csActor, TrackingImportCommitInput{Assignments: []TrackingAssignment{
		{OrderID: a.ID, LabelURL: lblB},                         // label only
		{OrderID: b.ID, LabelURL: lblC},                         // overwrite (client opted in)
		{OrderID: c.ID, TrackingNumber: "TN-C", LabelURL: lblA}, // both
		{OrderID: 99999, LabelURL: lblA},                        // unknown order
		{OrderID: a.ID + 1000, LabelURL: "ftp://bad"},           // invalid link
	}})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Updated != 3 || len(res.Failed) != 2 {
		t.Fatalf("result: %+v", res)
	}
	var oa, ob, oc models.Order
	svc.repo.DB.First(&oa, a.ID)
	svc.repo.DB.First(&ob, b.ID)
	svc.repo.DB.First(&oc, c.ID)
	if oa.ExternalLabelURL != lblB || oa.TrackingNumber != "" || oa.TrackingStatus != models.TrackingNone {
		t.Errorf("A: label only, tracking untouched: %+v", oa)
	}
	if ob.ExternalLabelURL != lblC {
		t.Errorf("B not overwritten: %q", ob.ExternalLabelURL)
	}
	if oc.ExternalLabelURL != lblA || oc.TrackingNumber != "TN-C" {
		t.Errorf("C: %+v", oc)
	}
	var audits int64
	svc.repo.DB.Model(&models.AuditLog{}).Where("action = ?", "ORDER_EXTERNAL_LABEL_SET").Count(&audits)
	if audits != 3 {
		t.Errorf("want 3 audit entries, got %d", audits)
	}
}

func TestExternalLabel_CommitNeedsPermission(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	a := seedLabelOrder(t, svc, "100001", "SO-A", "")
	_, err := svc.CommitTrackingImport(Actor{ID: 9, Role: models.RoleSeller}, TrackingImportCommitInput{
		Assignments: []TrackingAssignment{{OrderID: a.ID, LabelURL: lblA}}})
	if err == nil {
		t.Fatal("a seller must not be able to attach labels")
	}
}

var pdfBytes = []byte("%PDF-1.4\n1 0 obj\n<<>>\nendobj\ntrailer\n<<>>\n%%EOF\n")

func TestExternalLabel_Print(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.pdf":
			w.Header().Set("Content-Type", "application/octet-stream") // carriers often lie
			w.Write(pdfBytes)
		case "/login":
			w.Write([]byte("<html>login</html>"))
		case "/huge.pdf":
			w.Write([]byte("%PDF-"))
			w.Write(bytes.Repeat([]byte("x"), int(maxExternalLabelBytes)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	_, svc := newTrackingImportFixture(t)
	svc.extLabelClient = srv.Client()
	ok := seedLabelOrder(t, svc, "100001", "SO-A", srv.URL+"/ok.pdf")
	html := seedLabelOrder(t, svc, "100002", "SO-B", srv.URL+"/login")
	gone := seedLabelOrder(t, svc, "100003", "SO-C", srv.URL+"/missing.pdf")
	huge := seedLabelOrder(t, svc, "100004", "SO-D", srv.URL+"/huge.pdf")
	none := seedLabelOrder(t, svc, "100005", "SO-E", "")
	ctx := context.Background()

	f, err := svc.ExternalLabel(ctx, csActor, ok.ID)
	if err != nil || f.ContentType != "application/pdf" || !bytes.Equal(f.Data, pdfBytes) || f.Filename != "label-100001.pdf" {
		t.Fatalf("ok label: %+v %v", f, err)
	}
	for name, id := range map[string]uint{"html": html.ID, "404": gone.ID, "huge": huge.ID, "no label": none.ID} {
		if _, err := svc.ExternalLabel(ctx, csActor, id); err == nil {
			t.Errorf("%s must fail", name)
		}
	}
	if _, err := svc.ExternalLabel(ctx, csActor, 424242); err == nil {
		t.Error("unknown order must fail")
	}
	if _, err := svc.ExternalLabel(ctx, Actor{ID: 9, Role: models.RoleSeller}, ok.ID); err == nil {
		t.Error("a seller must not print internal labels")
	}
}

// The default client is the SSRF guard: a link to a private address (the
// operator's file is untrusted input) is refused before any connection.
func TestExternalLabel_PrintRefusesPrivateAddress(t *testing.T) {
	_, svc := newTrackingImportFixture(t)
	o := seedLabelOrder(t, svc, "100001", "SO-A", "http://169.254.169.254/latest/meta-data")
	if _, err := svc.ExternalLabel(context.Background(), csActor, o.ID); err == nil {
		t.Fatal("private address must be refused")
	}
}

// A link pasted into Excel often shows a word ("Xem label") with the URL behind
// the cell: the parser must read the real target.
func TestExternalLabel_XLSXHyperlinkCell(t *testing.T) {
	f := excelize.NewFile()
	defer f.Close()
	sh := f.GetSheetName(0)
	_ = f.SetSheetRow(sh, "A1", &[]interface{}{"Mã đơn", "Link label"})
	_ = f.SetSheetRow(sh, "A2", &[]interface{}{"SO-A", "Xem label"})
	if err := f.SetCellHyperLink(sh, "B2", lblA, "External"); err != nil {
		t.Fatal(err)
	}
	_ = f.SetSheetRow(sh, "A3", &[]interface{}{"SO-B", lblB})
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		t.Fatal(err)
	}
	file, err := ParseTrackingXLSXFile(&buf)
	if err != nil || len(file.Rows) != 2 || file.Rows[0].LabelURL != lblA || file.Rows[1].LabelURL != lblB {
		t.Fatalf("hyperlink not resolved: %+v %v", file, err)
	}
}
