# Smart Campaign Composer

Pazarlamacının doğal dille yazdığı bir kampanya brief'ini Claude ile **hedef kitleye** ve **SMS + email metinlerine** çeviren; insan onayına sunan; onaydan sonra kuyruk üzerinden mock provider'a gönderip durumu **canlı** gösteren uçtan uca sistem.

> Gerçek kimseye mesaj gitmez: alıcılar seed ile üretilen sahte kullanıcılardır (`@example.test`, uydurma telefonlar) ve gönderim yalnızca bu repodaki mock provider'a yapılır.

## Hızlı başlangıç

Gereksinim: Docker (Compose v2). Go/Node kurmanıza gerek yok.

```bash
docker compose up --build        # veya: make up
```

| Servis | Adres |
|---|---|
| Arayüz (Vue) | http://localhost:8080 |
| API | http://localhost:8081 (`/healthz`, `/readyz`, `/metrics`) |
| Mock provider | http://localhost:9090 (`/stats`) |

İlk açılışta migration'lar çalışır ve **4000 sahte kullanıcı** seed edilir (deterministik).

**Claude ile mi, mock LLM ile mi?** `ANTHROPIC_API_KEY` boşsa API, yerleşik **mock LLM** ile çalışır (offline, deterministik). Gerçek Claude için:

```bash
cp .env.example .env     # ANTHROPIC_API_KEY=... satırını doldur (.env commit edilmez)
docker compose up --build
```

### Demo akışı

1. "Örnek brief'i kullan" → *Kitleyi çıkar*. Segment kuralı ve kaç kişiye gideceği (SMS/email izinli sayıları ayrı) görünür.
2. *Mesajları üret* → SMS ve email metni **stream** olarak akar. Brief'e `[mock:long]` eklerseniz mock LLM ilk taslakta SMS limitini bilerek aşar; "taslak kurallara uymadı, yeniden yazılıyor" onarım döngüsünü canlı görürsünüz.
3. Metni düzenleyin (karakter sayaçları, `{{first_name}}` değişken butonları). Sunucu kuralları yeniden doğrular.
4. *Onayla ve gönder* (iki adımlı onay). Dashboard kanal başına gönderim durumunu canlı gösterir.
5. Bitince `make stats`: mock provider'ın gerçekte kaç teslimat yaptığı. `max_deliveries_for_one_key` **1** olmalı (hiçbir mesaj iki kez gitmedi), `lost_replies_after_delivery` ve `idempotent_replays` ise retry'ın gerçekten devreye girdiğini gösterir.

## Mimari

```
 Vue (nginx)  ──/api──►  Go API ──────────────► Claude API (tool use, JSON)
   │  fetch-SSE            │   ▲  doğrulama + onarım döngüsü
   │                       │   │
   │                       ▼   │
   │                  PostgreSQL  ◄──── sender.Engine (kanal başına worker havuzu)
   │                  users · campaigns · messages(kuyruk)      │  ratelimit.Pacer (100/sn/kanal)
   └── canlı stats (SSE) ◄─┘                                    ▼
                                                      mock provider (5xx · 429 · gecikme · kayıp yanıt)
```

| Paket | Sorumluluk |
|---|---|
| `internal/segment` | Segment kural dili (JSON ağacı), **whitelist** doğrulama, parametreli SQL'e derleme |
| `internal/content` | Kanal limitleri (SMS 160, konu 80), değişken doğrulama, kişiselleştirme sonrası en kötü durum uzunluğu |
| `internal/llm` | Claude istemcisi (HTTP, tool use, SSE), **doğrula → onar** döngüsü, deterministik mock |
| `internal/store` | Tek Postgres katmanı: migration, seed, onayda kuyruğu oluşturma, `SKIP LOCKED` ile claim |
| `internal/sender` | Kuyruk motoru: worker'lar, retry/backoff, 429, idempotency, izin kontrolü |
| `internal/ratelimit` | Katı hız sınırlayıcı (pacer) |
| `internal/httpapi` | REST + SSE uçları, `/healthz` `/readyz` `/metrics` |
| `cmd/mockprovider` | Sahte SMS/email gateway (hata enjeksiyonu + `Idempotency-Key`) |
| `web/` | Vue 3 arayüz (brief → kitle → stream → düzenle → onay → canlı dashboard) |

### API

| Uç | Açıklama |
|---|---|
| `POST /api/campaigns` `{brief}` | Claude'dan segment kuralı ister, doğrular, kitle sayısını hesaplar |
| `POST /api/campaigns/{id}/compose` | SMS + email metnini **SSE** ile stream eder (`partial` · `retry` · `final` · `error`) |
| `PUT /api/campaigns/{id}/content` | İnsan düzenlemesi; sunucuda yeniden doğrulanır (422 + `problems`) |
| `POST /api/campaigns/{id}/approve` | Kuyruğu oluşturur; idempotent |
| `GET /api/campaigns/{id}/events` | Canlı gönderim durumu (SSE) |
| `GET /healthz` · `/readyz` · `/metrics` | Canlılık, hazırlık (DB ping), Prometheus metrikleri |

## Testler

```bash
make test               # gofmt + go vet + go test -race (backend) ve vitest (frontend); Docker içinde
make test-integration   # Postgres gerektiren testler (docker compose db'sine karşı)
```

* **Gerçek API key gerekmez.** LLM tarafı `llm.Mock` ile test edilir; Anthropic istemcisi ise `httptest` sunucusuyla (başlıklar, tool_choice, SSE ayrıştırma) sınanır.
* Odak noktaları: segment injection denemeleri, LLM çıktısı onarım döngüsü, kanal limitleri, hız sınırı (1 sn'lik hiçbir pencerede >100), "yanıtı kaybolan istek" senaryosunda tam-bir-kez teslimat, izin kontrolü, state machine (onaydan sonra düzenleme yok, çift onay yok).

## Mock provider ile hata enjeksiyonu

`.env` / compose ortam değişkenleri: `PROVIDER_ERROR_RATE` (503), `PROVIDER_429_RATE`, `PROVIDER_TIMEOUT_RATE` (mesaj teslim edilir ama yanıt gecikir → istemci timeout'a düşer → aynı idempotency key ile retry), `PROVIDER_LIMIT_PER_SEC` (provider tarafı sert limit). Metrikler: `curl localhost:8081/metrics`.

## Kapsam ve bilinen sınırlar

Çekirdek akış tamamlandı. Yetiştirilemeyenler / bilinçli sadeleştirmeler (detay: [DECISIONS.md](DECISIONS.md)):

* **Bonuslar yok:** zamanlanmış gönderim, A/B varyantları, dile göre içerik, CI pipeline.
* SMS limiti **karakter sayısıyla** (160) uygulanır; GSM-7 / UCS-2 segment hesabı (Türkçe karakterli SMS'in gerçekte 70 karakterde bölünmesi) yapılmaz.
* Hız sınırlayıcı **süreç içi**dir: API'nin birden fazla kopyası çalıştırılırsa limit kopya başına uygulanır (paylaşımlı limitleyici gerekir).
* Kimlik doğrulama / çok kullanıcılı yetkilendirme yok.
* Segment kuralı arayüzde düzenlenemez (yalnızca yeniden üretilir); metin düzenlenebilir.
* `go.sum` repoya henüz eklenmedi: ilk `docker compose up --build` / `make tidy` üretir, ardından commit edilmelidir.

## Doğrulama durumu (dürüst not)

Backend'in Postgres'e bağlı katmanı (`internal/store`) bu geliştirme ortamında derlenemedi (Go modül proxy'si kapalıydı); bu yüzden: (1) tüm SQL deyimleri gerçek bir PostgreSQL 16'ya karşı `PREPARE/EXECUTE` ile ayrıca denendi (tür çıkarımı, tutarlı kuyruk oluşturma, tekrar onayda `INSERT 0 0`, izin filtresi, claim ve stale kurtarma), (2) kalan her paket `go vet` + `go test -race` ile ve gerçek mock provider'a karşı uçtan uca çalıştırıldı. `internal/store` entegrasyon testleri ve `docker compose up` bütünü ilk kez kullanıcının makinesinde çalıştırıldı/çalıştırılacak: `make test` ve `make test-integration` ile doğrulayın.
