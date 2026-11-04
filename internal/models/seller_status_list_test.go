package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAllSellerStatusesCoversEveryConst guards the single source of truth:
// every SellerStatus constant declared in enums.go must be listed in
// AllSellerStatuses (and rank above zero). Otherwise a new status silently
// drops out of HandedOverStatuses, the warranty SQL and every filter built on
// them.
func TestAllSellerStatusesCoversEveryConst(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "enums.go", nil, 0)
	if err != nil {
		t.Fatalf("parse enums.go: %v", err)
	}
	declared := map[string]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			id, ok := vs.Type.(*ast.Ident)
			if !ok || id.Name != "SellerStatus" {
				continue
			}
			for _, n := range vs.Names {
				declared[n.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no SellerStatus constants found in enums.go — test is not looking at the right file")
	}
	if len(AllSellerStatuses) != len(declared) {
		t.Fatalf("AllSellerStatuses has %d entries, enums.go declares %d SellerStatus constants (%v)", len(AllSellerStatuses), len(declared), declared)
	}
	seen := map[SellerStatus]bool{}
	for _, s := range AllSellerStatuses {
		if seen[s] {
			t.Fatalf("AllSellerStatuses lists %q twice", s)
		}
		seen[s] = true
		if s.Rank() <= 0 {
			t.Fatalf("SellerStatus %q has Rank() %d, want > 0", s, s.Rank())
		}
	}
}
