package middleware

import "testing"

// Boş kapsam kümesi HER YETENEĞİ verir ve bu bilerek böyledir — bootstrap anahtarı budur.
// AMA `system` bunun DIŞINDADIR: platformun kendi yeteneği, bir tenant admin'inin devraldığı
// bir şey değil. Bu test o istisnayı sabitler.
func TestEmptyScopeIsUnrestrictedButNeverSystem(t *testing.T) {
	admin := Scope{Scopes: nil}

	if !admin.HasScope("provision") {
		t.Fatal("boş küme provision'ı vermeli — bootstrap anahtarı bozulmamalı")
	}
	if admin.HasSystem() {
		t.Fatal("boş küme system'i VERMEMELİ: bir tenant admin anahtarı platform yetkisi taşıyamaz")
	}

	platform := Scope{Scopes: []string{ScopeSystem}}
	if !platform.HasSystem() {
		t.Fatal("system taşıyan anahtar HasSystem() dönmeli")
	}
	if platform.HasScope("provision") {
		t.Fatal("system taşıyan anahtar provision'ı OTOMATİK almamalı — kapsamlar ayrı")
	}
}
