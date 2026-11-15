# DECISIONS

Önemli kararlar ve nedenleri. Her başlık: **karar → neden → ödünleşim**.

## 1. LLM çıktısı asla doğrudan çalıştırılmaz: segment kural dili + whitelist

**Karar.** Claude SQL değil, küçük bir JSON kural ağacı üretir (`and`/`or` grupları, `field/operator/value` yaprakları). Backend: katı decode (bilinmeyen anahtar → hata), alan/operatör whitelist'i, tip kontrolü, derinlik ≤3, ≤20 koşul, değer sınırları. Geçerli kural **parametreli** SQL'e derlenir: yalnızca whitelist'teki alan adları sorgu metnine girer, her değer bind parametresidir.

**Neden.** Brief güvenilmez girdidir (prompt injection) ve model hata yapar. Güvenlik modeli "modele güven"e değil, "çıktıyı doğrula"ya dayanmalı. Şehir/dil gibi veri-bağımlı değerler DB'deki gerçek değerlere karşı doğrulanır ve kanonik yazıma çevrilir (`istanbul` → `İstanbul`, Türkçe büyük/küçük harf kuralıyla); uydurma şehir hata olarak Claude'a geri bildirilir.

**Ödünleşim.** İfade gücü sınırlı (join, alt sorgu, tarih aritmetiği yok). Yeni alan eklemek koddan geçer. Bu bilinçli: dar bir dil güvenli bir dildir.

## 2. Yapılandırılmış çıktı: zorunlu tool use + doğrula → onar döngüsü

**Karar.** Anthropic'e `tool_choice` ile belirli bir araç çağırtılır; yanıt o aracın JSON girdisidir. Backend yine de her şeyi doğrular (şema, limitler, değişkenler). Kural ihlalinde model **somut hata listesiyle** (ör. "sms_text 171 karakter, limit 160") en fazla 2 kez yeniden çağrılır; hâlâ geçersizse kullanıcıya hata gösterilir.

**Neden.** "JSON döndür" yalnızca dilek; garanti backend doğrulamasıdır. Onarım döngüsü, nadir ama gerçek hataları kullanıcıya yansıtmadan çözer, hata mesajı modele dönünce genelde ilk onarımda düzelir.

**Asla yapmadığımız şey:** geçersiz metni sessizce kesmek (SMS ortasından kesilmiş bir cümle gönderilir ve kimse fark etmez). Kurala uymayan metin kaydedilmez.

## 3. Kişiselleştirme sonrası **en kötü durum** uzunluğu

**Karar.** `{{first_name}}` gibi değişkenler yerleşince metin uzar. Doğrulama, şablonu kitledeki **en uzun** ad/soyad/şehir değeriyle genişleterek yapılır (kitle sorgusu `max(char_length(...))` döner). Bu değerler hem Claude'un prompt'una ("en uzun ad 14 karakter") hem validator'a girer.

**Neden.** Şablon 150 karakter görünüp en uzun isimle 165 olursa limit ihlali alıcının ekranında ortaya çıkar. Onay anında SQL ayrıca render sonrası uzunluğu kontrol eder; aşan satır `skipped / content_exceeds_limit` olarak kaydedilir (gönderilmez, görünür kalır).

**Ödünleşim.** Muhafazakâr: tek bir çok uzun isim herkes için limiti daraltır.

## 4. Stream tasarımı

**Karar.** Claude'un `input_json_delta` parçaları backend'de birikir; her parçada eksik JSON'dan bilinen üç alanın o ana kadarki metni çıkarılıp (`ExtractPartial`) tarayıcıya `partial` event'i olarak gider. Akış bitince tam JSON doğrulanır; ancak o zaman `final` event'i gelir ve içerik kaydedilir. Doğrulama başarısızsa `retry` event'i (somut sorunlarla) gider ve metin baştan yazılır.

**Neden.** Kullanıcı yazıyı akarken görür, ama ekranda "nihai" gösterilen şey her zaman doğrulanmış olandır. SSE, WebSocket'e göre daha basit ve proxy dostu; POST gerektiği için `EventSource` yerine `fetch` + `ReadableStream` kullanıldı (kendi küçük ayrıştırıcısı test edilmiş).

## 5. Kuyruk: Postgres (`FOR UPDATE SKIP LOCKED`), Redis/RabbitMQ değil

**Karar.** `messages` tablosu hem kuyruk hem denetim kaydıdır. Worker'lar `UPDATE … WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)` ile atomik claim yapar.

**Neden.** (1) Kampanya onayı ve kuyruğa yazma **tek transaction**: "onaylandı ama mesajlar kuyruğa girmedi" durumu yok. (2) Dayanıklılık ve görünürlük bedava: durum, hata, deneme sayısı sorgulanabilir; dashboard doğrudan buradan okur. (3) Daha az hareketli parça (tek komut kurulum). (4) Bu ölçekte (binlerce–yüz binlerce mesaj, 100/sn/kanal) Postgres kuyruğu fazlasıyla yeterli.

**Ödünleşim.** Milyonlarca mesaj/dk ölçeğinde veya çok düşük gecikme için özel bir broker gerekir; tasarım `sender.Store` arayüzü arkasında olduğundan taşınabilir.

## 6. Tam-bir-kez etkisi: unique kısıt + idempotency key

Üç katman:

1. **Kuyruğa girişte:** `UNIQUE(campaign_id, user_id, channel)` + `ON CONFLICT DO NOTHING`. Onay iki kez çağrılsa da (çift tık, tekrar istek) aynı kişiye aynı mesaj ikinci kez kuyruğa girmez. Onay ayrıca kampanya satırı kilidi (`FOR UPDATE`) altında yapılır.
2. **Provider'a giderken:** her deneme aynı `Idempotency-Key` (`kampanya:kullanıcı:kanal`) ile gider.
3. **Yanıt kaybolursa:** provider mesajı teslim eder ama yanıt zaman aşımına uğrar → biz yeniden deneriz → provider aynı key'i görüp **yeniden teslim etmeden** ilk sonucu döner. Mock provider bu durumu `PROVIDER_TIMEOUT_RATE` ile bilerek üretir; `/stats` içindeki `max_deliveries_for_one_key = 1` bunun kanıtıdır.

Kuyruk semantiği at-least-once'tır; idempotency key'le birleşince etki tam-bir-kezdir. Bir worker çökerse `sending` satırı 60 sn sonra `pending`'e döner ve aynı key'le yeniden denenir. Sonuç yazımları `AND status='sending'` korumalıdır.

## 7. Retry stratejisi

| Sonuç | Davranış |
|---|---|
| 2xx | `sent` |
| 5xx, 408, ağ hatası, timeout | Üstel backoff (2 sn × 2ⁿ, tavan 2 dk, ±%20 jitter), en çok **5 deneme**, sonra `failed` |
| **429** | Mesaj `Retry-After` kadar bekler ve **deneme hakkı harcamaz** (ayrı sayaç, en çok 50); kanal ayrıca kısa süre (≤500 ms) yavaşlar |
| Diğer 4xx | Kalıcı hata: `failed`, **retry yok** (tekrar denemek sonucu değiştirmez) |

429'da kanal duraklatması bilinçli olarak sınırlı: aksi halde %3 gibi rastgele 429 oranı kanalı sürekli 1 sn durdururdu. Mesajın kendisi ise tam `Retry-After` kadar bekler. Sonuçlar kapanış sırasında bile yazılabilsin diye iptal edilmeyen context ile kaydedilir (gönderilmiş mesaj "gönderilmedi" sanılıp tekrar gitmesin).

## 8. Hız sınırı: token bucket yerine "pacer"

Kanal başına süreç içi pacer, istekleri en az `1/100 sn` aralıkla dizer. Token bucket `burst` kadar fazla gönderime izin verir; "**saniyede en fazla 100**" şartını her 1 sn'lik pencerede garanti etmek için bursts'süz aralıklı yaklaşım seçildi (testte 1000 gönderimde her pencerede ≤100 doğrulanıyor). Limit **kanal başına** ayrı, SMS ve email birbirini yavaşlatmaz.

**Sınır.** Süreç içi: birden fazla API kopyasında limit kopya başına olur. Çözüm (kapsam dışı): paylaşımlı limitleyici (Redis/Postgres tabanlı) ya da tek bir sender kopyası.

## 9. İzin (consent) iki noktada uygulanır

Kuyruğa yazarken yalnızca o **kanala** izin vermiş kullanıcılar seçilir (SMS izni emaili, email izni SMS'i açmaz). Ayrıca claim sırasında `users` ile join edilip izin **tekrar** okunur; onaydan sonra izni geri çeken kullanıcıya mesaj gitmez (`skipped / no_consent`). Kitle ekranı kanal başına ulaşılabilir sayıyı ayrı gösterir.

## 10. İnsan onayı bir durum makinesidir

`segment_ready → content_ready → sending → completed`. Onaydan sonra metin değiştirilemez (409), içerik olmadan onay verilemez (409), onay idempotenttir, onay anında kayıtlı metin güncel kitleye karşı **yeniden** doğrulanır. Arayüz de iki adımlı onay ister ve onaydan sonra düzenleyiciyi salt-okunur yapar. Sunucu kuralları arayüze güvenmez.

## 11. Test edilebilirlik

* LLM bir **arayüzdür** (`llm.Client`); `llm.Mock` deterministik çalışır ve testlerde "kötü çıktı" üretmek için kancalar sunar. Anthropic istemcisi `httptest` ile sınanır. Hiçbir test gerçek API key veya ağ gerektirmez. Aynı mock, key yokken `docker compose up` ile uygulamanın demosunu da çalıştırır.
* Postgres sürücüsüne bağlı kod tek pakete (`store`) izole; büyük SQL deyimleri sürücüsüz `sqlgen` paketinde, bu sayede gerçek bir PostgreSQL'e karşı bağımsız denenebildi. Sender, `sender.Store` arayüzü + bellek içi sahte ile test edilir (çökme, 429, kayıp yanıt senaryoları).
* Postgres'e özgü davranışlar (claim'in aynı satırı iki kez vermemesi, çift onay, izin filtresi, stale kurtarma) `make test-integration` ile ayrı koşar; her test kendi şemasında çalışır.

## 12. Gözlemlenebilirlik

`/healthz` (süreç), `/readyz` (DB ping), `/metrics` (Prometheus metin formatı, kütüphanesiz: gönderilen/başarısız/retry/429/atlanan sayaçları, kuyruk derinliği gauge'u) ve kampanya başına SSE ile canlı durum. Dashboard yeniden bağlanır; sayfa yenilense bile URL'deki kampanya id'sinden geri yüklenir.

## 13. Teknoloji seçimleri

* **Go + `net/http`** (Go 1.22 mux kalıpları): eşzamanlılık (worker havuzu, pacer) için doğal, bağımlılık sayısı minimum (yalnızca `pgx`). Anthropic için SDK yerine ince bir HTTP istemcisi: akış ayrıştırması ve hata davranışı tamamen test edilebilir.
* **Vue 3 + Vite**, bileşen başına tek sorumluluk; mantık (`limits.js`, `sse.js`) bileşenlerden ayrı test edilir.
* Arayüz dili Türkçe (brief Türkçe, hedef kullanıcı Türk pazarlamacı).

## 14. Model zorunlu araç seçimini desteklemezse

Yapılandırılmış çıktı `tool_choice: {type: "tool"}` ile zorlanıyor. Gerçek API'ye karşı denemede kullanılan model bunu 400 ile reddetti. İstemci bu hatayı görünce bir kez `tool_choice: "auto"` ile yeniden dener ve system prompt'a "yalnızca bu aracı bir kez çağır" talimatını ekler; karar süreç boyunca hatırlanır (`atomic.Bool`), sonraki isteklerde ek tur olmaz. Güvenlik değişmez: model aracı çağırmazsa ya da geçersiz çıktı verirse mevcut doğrulama + onarım döngüsü (§2) devreye girer, çıktı yine whitelist'e karşı doğrulanır. Test: `TestAnthropicFallsBackToAutoToolChoice`.

## Yetiştirilemeyenler / sonraki adımlar

Zamanlanmış gönderim, A/B varyantları, dile göre içerik, CI pipeline; GSM-7/UCS-2 SMS segment hesabı; paylaşımlı rate limiter; kimlik doğrulama; segment kuralının arayüzde düzenlenmesi.
