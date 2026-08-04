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

// BİR ANAHTAR, KENDİ TAŞIMADIĞI BİR YETENEĞİ VEREMEZ.
//
// Okuma tarafı zaten doğruydu: HasSystem boş kümeyi dışlıyor, rotalar onunla kapılı. Eksik olan YAZMA
// tarafıydı — bir anahtar mintlerken istenen kapsamlar gövdeden geliyor ve rotanın kapısı `provision`
// diyor, ki o kapı "bu çağıran anahtar üretebilir" der, "İÇİNE ne koyabilir" demez.
//
// CanGrant'in iki kolu var ve ikisi de burada sürülüyor, çünkü yalnız reddi yazmak verme yolunun
// kırıldığını göstermez.
func TestAKeyCannotGrantACapabilityItDoesNotHold(t *testing.T) {
	// (1) REDDİN İKİ HÂLİ. Boş küme HER ordinary yeteneği verir — ama `system`'i ASLA, ve bu ikisinin
	// aynı anahtarda birlikte olması testin bütün noktası: HasScope ile yazılmış bir kontrol burada
	// sessizce true dönerdi.
	admin := Scope{Scopes: nil}
	if !admin.CanGrant("provision") {
		t.Fatal("boş küme ordinary bir yeteneği verebilmeli — bootstrap anahtarı anahtar mintleyebilir")
	}
	if admin.CanGrant(ScopeSystem) {
		t.Fatal("BOŞ KÜME system VEREMEZ: en az kapsamlı anahtar her şeyi dağıtan anahtar olamaz")
	}

	// (2) Adı konmuş ama system taşımayan bir anahtar da veremez.
	tenant := Scope{Scopes: []string{"provision", "approve"}}
	if tenant.CanGrant(ScopeSystem) {
		t.Fatal("provision taşıyan bir anahtar system veremez")
	}
	if !tenant.CanGrant("approve") {
		t.Fatal("taşıdığı yeteneği verebilmeli")
	}
	if tenant.CanGrant("publish") {
		t.Fatal("taşımadığı ordinary bir yeteneği de veremez")
	}

	// (3) VE VERME YOLU ÇALIŞIYOR. Bunsuz kural "hiçbir şey verilemez" diye de sağlanabilirdi.
	platform := Scope{Scopes: []string{ScopeSystem}}
	if !platform.CanGrant(ScopeSystem) {
		t.Fatal("system taşıyan anahtar system verebilmeli — aksi hâlde platform ikinci bir anahtar üretemez")
	}
}
