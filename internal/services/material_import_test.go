package services

import (
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newCatalogDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Batches/batch items too: deleting a material checks whether production still
	// references it.
	if err := db.AutoMigrate(&models.Material{}, &models.SKU{}, &models.SKUMaterial{},
		&models.Batch{}, &models.BatchItem{},
		// Orders too: deleting a SKU checks whether an order line still points at it.
		&models.Order{}, &models.OrderItem{}, &models.AuditLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func catalogSvc(db *gorm.DB) *CatalogService {
	repo := repositories.New(db)
	return &CatalogService{repo: repo, audit: &AuditService{repo: repo}}
}

func ptrInt(n int) *int { return &n }

// derefF prints a size for a failure message (nil-safe).
func derefF(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// sizeRow is a MaterialImportRow with both sides declared.
func sizeRow(row int, name string, l, w float64, desc string) MaterialImportRow {
	return MaterialImportRow{RowNumber: row, Material: name, LengthMM: &l, WidthMM: &w, Description: desc}
}

func TestParseMaterialImportFile(t *testing.T) {
	csv := strings.Join([]string{
		"Loại VL,Dài (mm),Rộng (mm),Mô tả,Định mức",
		"Mica trong 3 ly,1220,2440,Mica 3mm,20",
		"Gỗ 5 ly,,,,",       // blank size + blank desc
		"Hỏng,4 ft,8 ft,x,", // not millimetres → parse error
	}, "\n")
	rows, perrs, notices, err := ParseMaterialImportFile("CSV", strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 valid rows, got %d: %+v", len(rows), rows)
	}
	if rows[0].Material != "Mica trong 3 ly" || !dimEq(rows[0].LengthMM, 1220) || !dimEq(rows[0].WidthMM, 2440) || rows[0].Description != "Mica 3mm" {
		t.Fatalf("row0 wrong: %+v", rows[0])
	}
	if rows[1].LengthMM != nil || rows[1].WidthMM != nil {
		t.Fatalf("blank size should parse to nil, got %+v", rows[1])
	}
	if len(perrs) != 1 || perrs[0].ErrorCode != errDimInvalid {
		t.Fatalf("want 1 DIM_INVALID error, got %+v", perrs)
	}
	// The old template's quota column is named, not silently swallowed.
	if len(notices) != 1 || !strings.Contains(notices[0], "Định mức") {
		t.Fatalf("want a notice about the ignored quota column, got %v", notices)
	}

	// A combined "D x R" column works too; a file with no size column at all is
	// refused (that is the old quota template — nothing here to import).
	rows, _, _, err = ParseMaterialImportFile("CSV", strings.NewReader("Loại VL,Kích thước (mm)\nMica,1220 x 2440\n"))
	if err != nil || len(rows) != 1 || !dimEq(rows[0].LengthMM, 1220) || !dimEq(rows[0].WidthMM, 2440) {
		t.Fatalf("combined size column: %+v (%v)", rows, err)
	}
	if _, _, _, err := ParseMaterialImportFile("CSV", strings.NewReader("Loại VL,Định mức\nMica,20\n")); err == nil {
		t.Fatalf("a file without a size column must be refused")
	}
}

// TestMaterialImport_PreviewCommit covers create (with size + description),
// create-blank (no size), size update, description-only update, and blank cells
// never clearing an existing value.
func TestMaterialImport_PreviewCommit(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	seed := func(code, name string, l, w float64) *models.Material {
		m := &models.Material{Code: code, Name: name, LengthMM: &l, WidthMM: &w}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
		return m
	}
	mica := seed("MICA-TRONG-3-LY", "Mica trong 3 ly", 600, 900)
	keep := seed("KEEP", "Giữ nguyên", 100, 100)
	descOnly := seed("DESC-ONLY", "Mô tả mới", 50, 50)

	rows := []MaterialImportRow{
		sizeRow(1, "Mica trong 3 ly", 1220, 2440, ""),                             // update size
		sizeRow(2, "Gỗ 5 ly", 600, 900, "Gỗ tốt"),                                 // create w/ size + desc
		{RowNumber: 3, Material: "Acrylic"},                                       // create, no size, no desc
		{RowNumber: 4, Material: "Giữ nguyên"},                                    // blank → no change
		{RowNumber: 5, Material: "Mô tả mới", Description: "Ghi chú"},             // desc-only update, size untouched
		{RowNumber: 6, Material: "Nửa vời", LengthMM: ptrFloat(100)},              // one side only → error
		{RowNumber: 7, Material: "", LengthMM: ptrFloat(1), WidthMM: ptrFloat(1)}, // no name → error
	}

	pv, err := svc.PreviewMaterialImport("nvl.csv", rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if s := pv.Summary; s.NewMaterials != 2 || s.Updates != 2 || s.Unchanged != 1 || s.ErrorRows != 2 {
		t.Fatalf("summary = %+v, want 2 new / 2 updates / 1 unchanged / 2 errors", s)
	}
	codes := map[string]string{}
	for _, e := range pv.Errors {
		codes[e.Material] = e.ErrorCode
	}
	if codes["Nửa vời"] != errDimInvalid || codes[""] != errMaterialBlank {
		t.Fatalf("errors = %+v", pv.Errors)
	}
	for _, it := range pv.Items {
		if it.Name == "Mica trong 3 ly" && (it.Action != importActionUpdate || !dimEq(it.CurrentLengthMM, 600) || !dimEq(it.LengthMM, 1220)) {
			t.Fatalf("Mica item = %+v", it)
		}
	}

	res, err := svc.CommitMaterialImport(owner, rows)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if res.Applied.Created != 2 || res.Applied.Updated != 2 {
		t.Fatalf("applied = %+v", res.Applied)
	}
	// Fresh struct per lookup: GORM folds a non-zero primary key on the
	// receiver into the WHERE, so reusing one would AND two ids together.
	load := func(where string, arg any) models.Material {
		var m models.Material
		if err := db.Where(where, arg).First(&m).Error; err != nil {
			t.Fatalf("load %v: %v", arg, err)
		}
		return m
	}
	if got := load("id = ?", mica.ID); !dimEq(got.LengthMM, 1220) || !dimEq(got.WidthMM, 2440) {
		t.Fatalf("size not updated: %v × %v", derefF(got.LengthMM), derefF(got.WidthMM))
	}
	if got := load("id = ?", keep.ID); !dimEq(got.LengthMM, 100) || !dimEq(got.WidthMM, 100) {
		t.Fatalf("blank cells cleared a size: %v × %v", derefF(got.LengthMM), derefF(got.WidthMM))
	}
	if got := load("id = ?", descOnly.ID); got.Description != "Ghi chú" || !dimEq(got.LengthMM, 50) {
		t.Fatalf("desc-only update: desc=%q size=%v", got.Description, derefF(got.LengthMM))
	}
	if got := load("name = ?", "Gỗ 5 ly"); !dimEq(got.LengthMM, 600) || !dimEq(got.WidthMM, 900) || got.Description != "Gỗ tốt" || got.Code != "GO-5-LY" {
		t.Fatalf("created material = %+v", got)
	}
	if got := load("name = ?", "Acrylic"); got.LengthMM != nil || got.WidthMM != nil {
		t.Fatalf("blank size on create must stay undeclared, got %v × %v", derefF(got.LengthMM), derefF(got.WidthMM))
	}

	// Re-importing the same file is a no-op.
	pv2, err := svc.PreviewMaterialImport("nvl.csv", rows, nil, nil)
	if err != nil {
		t.Fatalf("re-preview: %v", err)
	}
	if pv2.Summary.NewMaterials != 0 || pv2.Summary.Updates != 0 || pv2.Summary.Unchanged != 5 {
		t.Fatalf("re-import should be all NOCHANGE, got %+v", pv2.Summary)
	}
}

// TestMaterialImport_OneMaterialPerName locks in what a material IS to this
// import: its name. Repeats of a name fold into one line when they agree (a
// later line may fill a blank), and are an error when they disagree — the file
// is never allowed to grow same-name variants the way the old quota import did.
// A catalog that already holds several materials with one name is reported too,
// instead of one of them being picked in silence.
func TestMaterialImport_OneMaterialPerName(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	for _, code := range []string{"CERAMIC", "CERAMIC-2"} {
		if err := db.Create(&models.Material{Code: code, Name: "Ceramic"}).Error; err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rows := []MaterialImportRow{
		sizeRow(1, "Mica trong 3 ly", 1220, 2440, "Mica 3mm"),
		sizeRow(2, "Mica trong 3 ly", 1220, 2440, "Mica 3mm"),    // exact dup
		sizeRow(3, "  mica TRONG 3 ly ", 1220, 2440, "mica 3MM"), // dup modulo case/spaces
		{RowNumber: 4, Material: "Mica trong 3 ly"},              // blank repeat → folds, adds nothing
		{RowNumber: 5, Material: "Basswood 5mm"},                 // blank first…
		sizeRow(6, "Basswood 5mm", 600, 900, ""),                 // …a later line fills the size
		sizeRow(7, "Mica 2 ly", 600, 900, ""),
		sizeRow(8, "Mica 2 ly", 1220, 2440, ""), // same name, different size → conflict
		sizeRow(9, "Ceramic", 300, 300, ""),     // catalog already has two "Ceramic"
	}
	pv, err := svc.PreviewMaterialImport("dup.csv", rows, nil, nil)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(pv.Items) != 2 {
		t.Fatalf("items = %+v, want Mica trong 3 ly + Basswood 5mm", pv.Items)
	}
	if pv.Summary.DuplicateRows != 4 {
		t.Fatalf("duplicate rows = %d, want 4 (rows 2, 3, 4, 6 folded)", pv.Summary.DuplicateRows)
	}
	if got := pv.Items[0].RowNumbers; len(got) != 4 || got[0] != 1 || got[3] != 4 {
		t.Fatalf("first item rows = %v, want rows 1–4 folded together", got)
	}
	if bw := pv.Items[1]; !dimEq(bw.LengthMM, 600) || !dimEq(bw.WidthMM, 900) {
		t.Fatalf("a later line must fill the size a blank line left: %+v", bw)
	}
	codes := map[string]string{}
	for _, e := range pv.Errors {
		codes[e.Material] = e.ErrorCode
		if e.ErrorCode == errMaterialRowConflict && len(e.RowNumbers) != 2 {
			t.Fatalf("a conflict must point at every row involved: %+v", e)
		}
	}
	if codes["Mica 2 ly"] != errMaterialRowConflict || codes["Ceramic"] != errMaterialAmbiguous {
		t.Fatalf("errors = %+v", pv.Errors)
	}

	owner := Actor{ID: 1, Role: models.RoleOwner}
	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("commit: %v", err)
	}
	repo := repositories.New(db)
	if mats, _ := repo.Material.ListByNameInsensitive("Mica trong 3 ly"); len(mats) != 1 {
		t.Fatalf("materials named Mica trong 3 ly = %d, want exactly 1", len(mats))
	}
	if mats, _ := repo.Material.ListByNameInsensitive("Mica 2 ly"); len(mats) != 0 {
		t.Fatalf("a conflicting material must not be created, got %d", len(mats))
	}
	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("re-commit: %v", err)
	}
	if again, _ := repo.Material.ListByNameInsensitive("Mica trong 3 ly"); len(again) != 1 {
		t.Fatalf("re-import must not duplicate the catalog, got %d materials", len(again))
	}
}

// TestMaterialImport_StaysOffTheRowByRowPath is the N+1 guard. The catalog lookup
// used to run once per line and the commit probed for a free code per new
// material: a 350-row file meant ~700 round-trips, which on a hosted database is
// minutes of waiting. Preview must read the catalog in one query and commit must
// insert in batches, no matter how many rows the file has.
func TestMaterialImport_StaysOffTheRowByRowPath(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	owner := Actor{ID: 1, Role: models.RoleOwner}

	const materials = 300
	rows := make([]MaterialImportRow, 0, materials*2)
	for i := 0; i < materials; i++ {
		name := "NVL " + strconv.Itoa(i)
		size := float64(100 + i%5)
		rows = append(rows,
			sizeRow(len(rows)+1, name, size, size, ""),
			// Every material repeated once — the fold must not cost extra queries.
			sizeRow(len(rows)+2, name, size, size, ""),
		)
	}

	stmts := 0
	count := func(*gorm.DB) { stmts++ }
	for _, reg := range []func(string, func(*gorm.DB)) error{
		db.Callback().Query().After("gorm:query").Register,
		db.Callback().Create().After("gorm:create").Register,
		db.Callback().Update().After("gorm:update").Register,
		db.Callback().Row().After("gorm:row").Register,
		db.Callback().Raw().After("gorm:raw").Register,
	} {
		if err := reg("test:count", count); err != nil {
			t.Fatalf("register callback: %v", err)
		}
	}

	if _, err := svc.PreviewMaterialImport("big.csv", rows, nil, nil); err != nil {
		t.Fatalf("preview: %v", err)
	}
	if stmts > 2 {
		t.Fatalf("preview of %d rows issued %d statements, want ≤2 (one catalog read)", len(rows), stmts)
	}

	stmts = 0
	if _, err := svc.CommitMaterialImport(owner, rows); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// 1 catalog read + 1 code read + ⌈300/200⌉ inserts + the audit log.
	if stmts > 8 {
		t.Fatalf("commit of %d new materials issued %d statements, want a handful of batches", materials, stmts)
	}

	repo := repositories.New(db)
	if got, _ := repo.Material.AllCodes(); len(got) != materials {
		t.Fatalf("created %d materials, want %d", len(got), materials)
	}
}

// TestMaterialSize_CRUD: the material form declares a sheet size the same way
// the SKU form declares a product size — both sides or neither, rounded to
// 0.01 mm, partial updates keep what they don't send — and the derived quota
// follows from the two.
func TestMaterialSize_CRUD(t *testing.T) {
	db := newCatalogDB(t)
	svc := catalogSvc(db)
	ops := Actor{ID: 2, Role: models.RoleOps} // no OWNER lever any more: sizes are master data

	if _, err := svc.CreateMaterial(ops, MaterialInput{Code: "MICA", Name: "Mica", LengthMM: ptrFloat(1220)}); err == nil {
		t.Fatalf("one side without the other must be refused")
	}
	m, err := svc.CreateMaterial(ops, MaterialInput{Code: "MICA", Name: "Mica", LengthMM: ptrFloat(1220.004), WidthMM: ptrFloat(2440)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !dimEq(m.LengthMM, 1220) || !dimEq(m.WidthMM, 2440) {
		t.Fatalf("size stored as %v × %v", m.LengthMM, m.WidthMM)
	}

	// A name-only edit keeps the size; clearing one side clears the pair only
	// when both are cleared — half a size is refused.
	if _, err := svc.UpdateMaterial(ops, m.ID, MaterialUpdateInput{Name: "Mica trong"}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, _ := svc.GetMaterial(m.ID); !dimEq(got.LengthMM, 1220) || got.Name != "Mica trong" {
		t.Fatalf("rename touched the size: %+v", got)
	}
	if _, err := svc.UpdateMaterial(ops, m.ID, MaterialUpdateInput{WidthMM: ptrFloat(0)}); err == nil {
		t.Fatalf("clearing one side only must be refused")
	}
	if _, err := svc.UpdateMaterial(ops, m.ID, MaterialUpdateInput{LengthMM: ptrFloat(0), WidthMM: ptrFloat(0)}); err != nil {
		t.Fatalf("clear both: %v", err)
	}
	if got, _ := svc.GetMaterial(m.ID); got.LengthMM != nil || got.WidthMM != nil {
		t.Fatalf("size not cleared: %+v", got)
	}

	// The quota of a pair is nobody's input: it comes from the two sizes.
	if _, err := svc.UpdateMaterial(ops, m.ID, MaterialUpdateInput{LengthMM: ptrFloat(100), WidthMM: ptrFloat(100)}); err != nil {
		t.Fatalf("set size: %v", err)
	}
	sku, err := svc.CreateSKU(ops, SKUInput{
		Code: "AO-3X5", Name: "AO 3x5", LengthMM: ptrFloat(10), WidthMM: ptrFloat(15),
		Materials: []SKUMaterialInput{{MaterialID: m.ID, QuantityPerUnit: 1}},
	})
	if err != nil {
		t.Fatalf("create sku: %v", err)
	}
	mat, _ := svc.GetMaterial(m.ID)
	// Nothing declared → grid estimate: ⌊100/10⌋·⌊100/15⌋ = 60 (not ⌊10000/150⌋ = 66).
	if got := models.ProductionQuota(sku, mat); got != 60 {
		t.Fatalf("quota = %d, want 10×6 = 60", got)
	}
}
