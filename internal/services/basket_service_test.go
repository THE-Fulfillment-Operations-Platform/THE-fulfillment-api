package services

import (
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"the-fulfillment/backend/internal/apperr"
	"the-fulfillment/backend/internal/models"
	"the-fulfillment/backend/internal/repositories"
)

func newBasketEnv(t *testing.T) (*gorm.DB, *BasketService) {
	t.Helper()
	db := newQCDB(t)
	repo := repositories.New(db)
	audit := &AuditService{repo: repo}
	return db, &BasketService{repo: repo, audit: audit, qc: &QCService{repo: repo, audit: audit}}
}

func wantHTTP(t *testing.T, name string, err error, status int) {
	t.Helper()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != status {
		t.Fatalf("%s: want HTTP %d, got %v", name, status, err)
	}
}

func passAll(db *gorm.DB, orderID uint) {
	db.Model(&models.OrderItem{}).Where("order_id = ?", orderID).Update("internal_status", models.StatusQCPassed)
}

var qcActor = Actor{ID: 1, Role: models.RoleQC}

// Hợp đồng chính: đơn nhiều sản phẩm, mới QC đạt 1 cái → tạo giỏ được số TRỐNG
// THẤP NHẤT; bấm lại (hoặc trạm khác bấm) vẫn ra đúng giỏ đó, không cấp thêm số.
func TestBasket_LowestFreeNumberAndIdempotent(t *testing.T) {
	db, svc := newBasketEnv(t)
	a := seedQCResultOrder(t, db, "100001", models.StatusQCPassed, models.StatusCut)
	b := seedQCResultOrder(t, db, "100002", models.StatusQCPassed, models.StatusPending, models.StatusPending)
	c := seedQCResultOrder(t, db, "100003", models.StatusCut, models.StatusCut)

	ga, created, err := svc.Create(qcActor, a.ID)
	if err != nil || !created || ga.Number != 1 || ga.Code != "GIO-1" {
		t.Fatalf("giỏ đầu tiên phải là số 1: %+v created=%v err=%v", ga, created, err)
	}
	if ga.Status != BasketOpen || ga.TotalItems != 2 || ga.PassedItems != 1 || len(ga.Items) != 2 {
		t.Fatalf("giỏ phải ĐANG GOM 1/2: %+v", ga)
	}
	again, created, err := svc.Create(qcActor, a.ID)
	if err != nil || created || again.Number != 1 {
		t.Fatalf("tạo lại cho cùng đơn phải trả giỏ cũ: %+v created=%v err=%v", again, created, err)
	}
	gb, _, _ := svc.Create(qcActor, b.ID)
	gc, _, _ := svc.Create(qcActor, c.ID)
	if gb.Number != 2 || gc.Number != 3 {
		t.Fatalf("số giỏ phải tăng dần 2, 3: %d, %d", gb.Number, gc.Number)
	}

	// Xoá giỏ 2 → đơn mới tiếp theo lấy lại số 2 (số trống thấp nhất), không phải 4.
	passAll(db, b.ID)
	if _, err := svc.Release(qcActor, gb.ID, ReleaseInput{}); err != nil {
		t.Fatalf("xoá giỏ đủ hàng: %v", err)
	}
	d := seedQCResultOrder(t, db, "100004", models.StatusQCPassed, models.StatusCut)
	gd, _, err := svc.Create(qcActor, d.ID)
	if err != nil || gd.Number != 2 {
		t.Fatalf("phải dùng lại số trống thấp nhất là 2, got %+v err=%v", gd, err)
	}

	// Chỉ mục duy nhất từng phần: hai giỏ sống không được trùng số.
	if err := db.Create(&models.Basket{Number: 1, OrderID: d.ID}).Error; err == nil {
		t.Fatalf("DB phải chặn 2 giỏ đang dùng trùng số")
	}
}

func TestBasket_RefusesOrdersThatDoNotNeedOne(t *testing.T) {
	db, svc := newBasketEnv(t)
	single := seedQCResultOrder(t, db, "100010", models.StatusCut)
	_, _, err := svc.Create(qcActor, single.ID)
	wantHTTP(t, "đơn 1 sản phẩm", err, 409)

	done := seedQCResultOrder(t, db, "100011", models.StatusQCPassed, models.StatusQCPassed)
	_, _, err = svc.Create(qcActor, done.ID)
	wantHTTP(t, "đơn đã QC đủ", err, 409)

	pending := seedQCResultOrder(t, db, "100012", models.StatusPending, models.StatusPending)
	db.Model(&models.Order{}).Where("id = ?", pending.ID).Update("review_status", models.ReviewPending)
	_, _, err = svc.Create(qcActor, pending.ID)
	wantHTTP(t, "đơn chưa duyệt", err, 409)

	gone := seedQCResultOrder(t, db, "100013", models.StatusCut, models.StatusCut)
	db.Model(&models.Order{}).Where("id = ?", gone.ID).Update("cancellation_status", models.CancellationApproved)
	_, _, err = svc.Create(qcActor, gone.ID)
	wantHTTP(t, "đơn đã huỷ", err, 409)

	_, _, err = svc.Create(qcActor, 99999)
	wantHTTP(t, "đơn không có", err, 404)
}

// Hết 100 giỏ thì báo rõ, không cấp số 101.
func TestBasket_AllBasketsInUse(t *testing.T) {
	db, svc := newBasketEnv(t)
	for n := 1; n <= models.MaxBaskets; n++ {
		if err := db.Create(&models.Basket{Number: n, OrderID: uint(100000 + n)}).Error; err != nil {
			t.Fatalf("seed giỏ %d: %v", n, err)
		}
	}
	o := seedQCResultOrder(t, db, "100020", models.StatusQCPassed, models.StatusCut)
	_, _, err := svc.Create(qcActor, o.ID)
	wantHTTP(t, "hết giỏ", err, 409)
	if !strings.Contains(err.Error(), "Hết giỏ trống") {
		t.Fatalf("thông báo phải nói hết giỏ: %v", err)
	}
}

// Trạng thái giỏ đọc từ đơn: QC đạt nốt → ĐỦ; QC fail / hạ QC làm đơn thiếu lại →
// ĐANG GOM; xoá giỏ chỉ được khi ĐỦ (hoặc đơn huỷ), còn thiếu thì chỉ admin xoá
// được và phải có lý do.
func TestBasket_FullAndReleaseRules(t *testing.T) {
	db, svc := newBasketEnv(t)
	o := seedQCResultOrder(t, db, "100030", models.StatusQCPassed, models.StatusCut)
	g, _, _ := svc.Create(qcActor, o.ID)

	_, err := svc.Release(qcActor, g.ID, ReleaseInput{})
	wantHTTP(t, "xoá giỏ chưa đủ", err, 409)
	_, err = svc.Release(qcActor, g.ID, ReleaseInput{Force: true, Reason: "lấy hàng ra"})
	wantHTTP(t, "QC ép xoá giỏ", err, 403)
	admin := Actor{ID: 2, Role: models.RoleAdmin}
	_, err = svc.Release(admin, g.ID, ReleaseInput{Force: true})
	wantHTTP(t, "ép xoá không lý do", err, 400)

	passAll(db, o.ID)
	fo, err := svc.ForOrder(o.ID)
	if err != nil || fo.Basket == nil || fo.Basket.Status != BasketFull {
		t.Fatalf("QC đạt đủ thì giỏ phải ĐỦ: %+v err=%v", fo, err)
	}
	rel, err := svc.Release(qcActor, g.ID, ReleaseInput{})
	if err != nil || rel.ReleasedAt == nil {
		t.Fatalf("giỏ đủ phải xoá được: %+v err=%v", rel, err)
	}
	_, err = svc.Release(qcActor, g.ID, ReleaseInput{})
	wantHTTP(t, "xoá lần hai", err, 409)
	if fo, _ := svc.ForOrder(o.ID); fo.Basket != nil {
		t.Fatalf("xoá xong đơn không còn giỏ")
	}

	// Admin ép xoá giỏ chưa đủ, có lý do.
	o2 := seedQCResultOrder(t, db, "100031", models.StatusQCPassed, models.StatusCut)
	g2, _, _ := svc.Create(qcActor, o2.ID)
	if _, err := svc.Release(admin, g2.ID, ReleaseInput{Force: true, Reason: "đơn đổi ý, gom lại sau"}); err != nil {
		t.Fatalf("admin ép xoá có lý do: %v", err)
	}

	// Đơn bị huỷ khi đang nằm giỏ → giỏ hiện ĐƠN HUỶ, xoá được không cần ép.
	o3 := seedQCResultOrder(t, db, "100032", models.StatusQCPassed, models.StatusCut)
	g3, _, _ := svc.Create(qcActor, o3.ID)
	db.Model(&models.Order{}).Where("id = ?", o3.ID).Update("cancellation_status", models.CancellationApproved)
	list, _ := svc.List()
	var st string
	for _, v := range list {
		if v.ID == g3.ID {
			st = v.Status
		}
	}
	if st != BasketCancelled {
		t.Fatalf("đơn huỷ thì giỏ phải CANCELLED, got %q", st)
	}
	if _, err := svc.Release(qcActor, g3.ID, ReleaseInput{}); err != nil {
		t.Fatalf("giỏ của đơn huỷ phải xoá được: %v", err)
	}
}

// Một ô quét ở màn Giỏ chờ nhận mọi thứ trên tay người làm: tem giỏ, mã đơn, mã
// người bán, tem sản phẩm.
func TestBasket_LookupByAnyCode(t *testing.T) {
	db, svc := newBasketEnv(t)
	o := seedQCResultOrder(t, db, "100040", models.StatusQCPassed, models.StatusCut)
	g, _, _ := svc.Create(qcActor, o.ID)

	for _, code := range []string{"GIO-1", "gio1", "GIỎ 1", "1", " 1 ", "100040", "SO-100040", "100040_1"} {
		res, err := svc.Lookup(code)
		if err != nil || res.Basket == nil || res.Basket.ID != g.ID {
			t.Fatalf("quét %q phải ra giỏ 1: %+v err=%v", code, res, err)
		}
	}
	_, err := svc.Lookup("GIO-7")
	wantHTTP(t, "giỏ trống", err, 404)
	_, err = svc.Lookup("999999")
	wantHTTP(t, "mã đơn không có", err, 404)

	// Đơn chưa có giỏ: màn hình được mời tạo giỏ.
	o2 := seedQCResultOrder(t, db, "100041", models.StatusQCPassed, models.StatusCut, models.StatusPending)
	res, err := svc.Lookup("100041")
	if err != nil || res.Basket != nil || !res.NeedsBasket || res.Order == nil || res.Order.TotalItems != 3 {
		t.Fatalf("đơn chưa có giỏ phải mời tạo giỏ: %+v err=%v", res, err)
	}
	_ = o2
}

// Lúc sang đóng hàng là chắc chắn phải đủ: đơn CÒN thiếu sản phẩm thì không gửi
// được (luật cũ chặn theo QC). Đơn đã đủ thì giỏ chờ không được cản đường — nếu giỏ
// chưa kịp tự xoá thì bước gửi tự dọn, rồi gửi bình thường.
func TestBasket_NeverHoldsACompleteOrder(t *testing.T) {
	db := newHandoffDB(t)
	// Giỏ đọc trạng thái QC của đơn (bảng qc_records + người kiểm).
	if err := db.AutoMigrate(&models.QCRecord{}, &models.User{}); err != nil {
		t.Fatal(err)
	}
	f := newFakeProvider()
	ship := newPackingService(db, newSync(t, db, f))
	repo := repositories.New(db)
	audit := &AuditService{repo: repo}
	svc := &BasketService{repo: repo, audit: audit, qc: &QCService{repo: repo, audit: audit}}

	o := seedOrderForShipping(t, db, "100050", models.StatusQCPassed, models.StatusCut)
	if _, _, err := svc.Create(qcActor, o.ID); err != nil {
		t.Fatalf("tạo giỏ: %v", err)
	}
	// Còn thiếu 1 sản phẩm: không gửi được.
	res, err := ship.ShipOrdersToCarrier(opsActor(), []uint{o.ID})
	if err != nil || len(res.Shipped) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("đơn chưa đủ phải bị chặn: %+v err=%v", res, err)
	}
	// Sản phẩm cuối đạt nhưng giỏ chưa kịp xoá → gửi được, giỏ được dọn.
	db.Model(&models.OrderItem{}).Where("order_id = ?", o.ID).Update("internal_status", models.StatusQCPassed)
	res, err = ship.ShipOrdersToCarrier(opsActor(), []uint{o.ID})
	if err != nil || len(res.Shipped) != 1 {
		t.Fatalf("đơn đủ hàng phải gửi được dù giỏ chưa kịp xoá: %+v err=%v", res, err)
	}
	if b, _ := repo.Basket.LiveByOrder(o.ID); b != nil {
		t.Fatalf("giỏ của đơn đã gửi phải được dọn")
	}
}

// Kết quả QC / Chờ gửi hàng thấy số giỏ của đơn.
func TestBasket_NumberShowsInQCResults(t *testing.T) {
	db, svc := newBasketEnv(t)
	o := seedQCResultOrder(t, db, "100060", models.StatusQCPassed, models.StatusCut)
	seedQCResultOrder(t, db, "100061", models.StatusCut)
	if _, _, err := svc.Create(qcActor, o.ID); err != nil {
		t.Fatalf("tạo giỏ: %v", err)
	}
	rows, _, _, err := svc.qc.QCResults(repositories.QCResultFilter{})
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	for _, r := range rows {
		switch r.InternalCode {
		case "100060":
			if r.BasketNumber == nil || *r.BasketNumber != 1 {
				t.Fatalf("đơn 100060 phải mang số giỏ 1: %+v", r.BasketNumber)
			}
		case "100061":
			if r.BasketNumber != nil {
				t.Fatalf("đơn không có giỏ không được mang số giỏ")
			}
		}
	}
}

func TestParseBasketCode(t *testing.T) {
	for code, want := range map[string]int{"GIO-37": 37, "gio 5": 5, "GIỎ-100": 100, "G12": 12, "37": 37, "#3": 3} {
		if n, ok := parseBasketCode(code); !ok || n != want {
			t.Errorf("%q → %d %v, want %d", code, n, ok, want)
		}
	}
	for _, code := range []string{"GIO-0", "GIO-101", "100040", "100040_1", "SO-1", ""} {
		if n, ok := parseBasketCode(code); ok {
			t.Errorf("%q không phải mã giỏ, got %d", code, n)
		}
	}
}

// Hợp đồng mới (20/10): sản phẩm CUỐI của đơn QC PASS xong thì giỏ TỰ XOÁ — không ai
// phải bấm. Đi qua QCService.Pass thật (cùng đường trạm QC gọi).
func TestBasket_AutoDeletedWhenLastProductPasses(t *testing.T) {
	db, svc := newBasketEnv(t)
	svc.qc.baskets = svc // QCService.Pass gọi svc.AutoRelease khi sản phẩm cuối đạt
	repo := svc.repo

	o := seedQCResultOrder(t, db, "100070", models.StatusQCPassed, models.StatusCut, models.StatusCut)
	db.Model(&models.OrderItem{}).Where("order_id = ?", o.ID).Update("mockup_url", "https://example.com/m.png")
	// QC chỉ cho PASS sản phẩm đã có phần sản xuất (batch) — dựng cho 2 sản phẩm chưa QC.
	batch := &models.Batch{Code: "B-101900", MaterialID: 1, Status: models.StatusCut}
	if err := db.Create(batch).Error; err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	var its []models.OrderItem
	db.Where("order_id = ? AND internal_status = ?", o.ID, models.StatusCut).Find(&its)
	for _, it := range its {
		if err := db.Create(&models.BatchItem{BatchID: batch.ID, OrderItemID: it.ID, MaterialID: 1, Status: models.StatusCut}).Error; err != nil {
			t.Fatalf("seed batch item: %v", err)
		}
	}
	g, _, err := svc.Create(qcActor, o.ID)
	if err != nil {
		t.Fatalf("tạo giỏ: %v", err)
	}
	if g.Number != 1 {
		t.Fatalf("giỏ đầu phải là số 1, got %d", g.Number)
	}

	pass := func(code string) {
		t.Helper()
		if _, err := svc.qc.Pass(qcActor, QCDecisionInput{ScanRef: ScanRef{Code: code}}); err != nil {
			t.Fatalf("QC pass %s: %v", code, err)
		}
	}
	// Sản phẩm thứ 2 đạt: còn thiếu 1 → giỏ vẫn còn.
	pass("100070_2")
	if b, _ := repo.Basket.LiveByOrder(o.ID); b == nil {
		t.Fatalf("còn thiếu 1 sản phẩm thì giỏ phải còn")
	}
	// Sản phẩm cuối đạt → đủ → giỏ tự xoá, số 1 trống ngay.
	pass("100070_3")
	if b, _ := repo.Basket.LiveByOrder(o.ID); b != nil {
		t.Fatalf("đơn đủ hàng thì giỏ phải tự xoá, vẫn còn giỏ #%d", b.Number)
	}
	var rel models.Basket
	db.First(&rel, g.ID)
	if rel.ReleasedAt == nil || rel.ReleaseNote != autoReleaseNote {
		t.Fatalf("giỏ phải ghi nhận là tự xoá: %+v", rel)
	}
	if used, _ := repo.Basket.LiveNumbers(); len(used) != 0 {
		t.Fatalf("không còn giỏ nào đang dùng, got %v", used)
	}

	// Đơn mới tạo giỏ: lấy lại ngay số 1.
	o2 := seedQCResultOrder(t, db, "100071", models.StatusQCPassed, models.StatusCut)
	g2, _, err := svc.Create(qcActor, o2.ID)
	if err != nil || g2.Number != 1 {
		t.Fatalf("số 1 vừa trống phải được cấp lại: %+v err=%v", g2, err)
	}
}

// Cấp số: quét 1..100, chỗ nào đang vắng thì lấy chỗ đó (thấp nhất). Cả 99 số kia
// đang có người dùng mà chỉ trống đúng một số → giỏ mới lấy đúng số đó.
func TestBasket_FillsTheOnlyFreeNumber(t *testing.T) {
	db, svc := newBasketEnv(t)
	for n := 1; n <= models.MaxBaskets; n++ {
		if n == 37 {
			continue
		}
		if err := db.Create(&models.Basket{Number: n, OrderID: uint(100000 + n)}).Error; err != nil {
			t.Fatalf("seed giỏ %d: %v", n, err)
		}
	}
	o := seedQCResultOrder(t, db, "100080", models.StatusQCPassed, models.StatusCut)
	g, _, err := svc.Create(qcActor, o.ID)
	if err != nil || g.Number != 37 {
		t.Fatalf("chỉ trống số 37 thì phải lấy 37, got %+v err=%v", g, err)
	}
	// Hết sạch số thì mới báo hết.
	o2 := seedQCResultOrder(t, db, "100081", models.StatusQCPassed, models.StatusCut)
	_, _, err = svc.Create(qcActor, o2.ID)
	wantHTTP(t, "hết giỏ", err, 409)
}

// Lưới an toàn: đơn đủ hàng mà giỏ chưa kịp xoá (sản phẩm cuối bị HUỶ chứ không QC)
// thì màn Giỏ chờ và bước gửi hàng tự dọn — không treo giỏ "đủ" chờ một cú bấm.
func TestBasket_SweepsCompleteOrders(t *testing.T) {
	db, svc := newBasketEnv(t)
	o := seedQCResultOrder(t, db, "100090", models.StatusQCPassed, models.StatusCut)
	if _, _, err := svc.Create(qcActor, o.ID); err != nil {
		t.Fatalf("tạo giỏ: %v", err)
	}
	var items []models.OrderItem
	db.Where("order_id = ?", o.ID).Order("line_no asc").Find(&items)
	// Sản phẩm còn lại bị huỷ → đơn chỉ còn 1 sản phẩm, đã QC đạt → đủ.
	db.Model(&models.OrderItem{}).Where("id = ?", items[1].ID).Update("cancellation_status", models.CancellationApproved)

	if reason := basketBlockReason(svc.repo, o.ID); reason != "" {
		t.Fatalf("đơn đủ hàng không bị giỏ chặn gửi: %q", reason)
	}
	if b, _ := svc.repo.Basket.LiveByOrder(o.ID); b != nil {
		t.Fatalf("kiểm tra gửi hàng phải dọn giỏ của đơn đủ")
	}

	o2 := seedQCResultOrder(t, db, "100091", models.StatusQCPassed, models.StatusCut)
	if _, _, err := svc.Create(qcActor, o2.ID); err != nil {
		t.Fatalf("tạo giỏ 2: %v", err)
	}
	var items2 []models.OrderItem
	db.Where("order_id = ?", o2.ID).Order("line_no asc").Find(&items2)
	db.Model(&models.OrderItem{}).Where("id = ?", items2[1].ID).Update("cancellation_status", models.CancellationApproved)
	list, err := svc.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("List phải dọn giỏ đủ hàng, còn %d giỏ, err=%v", len(list), err)
	}
}
