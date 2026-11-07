package store

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var firstNames = []string{"Ahmet", "Mehmet", "Mustafa", "Ali", "Hüseyin", "Hasan", "İbrahim", "Yusuf", "Emre", "Burak", "Can", "Cem", "Kerem", "Onur", "Serkan",
	"Ayşe", "Fatma", "Emine", "Hatice", "Zeynep", "Elif", "Merve", "Büşra", "Selin", "Deniz", "Gizem", "Esra", "Seda", "Pınar", "Ceren", "Ece", "İrem", "Melis", "Damla"}
var lastNames = []string{"Yılmaz", "Kaya", "Demir", "Şahin", "Çelik", "Yıldız", "Yıldırım", "Öztürk", "Aydın", "Özdemir", "Arslan", "Doğan", "Kılıç", "Aslan", "Çetin",
	"Kara", "Koç", "Kurt", "Özkan", "Şimşek", "Polat", "Korkmaz", "Erdoğan", "Güneş", "Aksoy", "Tekin", "Avcı", "Bozkurt"}

type cityWeight struct {
	name   string
	weight int
}

var cities = []cityWeight{{"İstanbul", 34}, {"Ankara", 14}, {"İzmir", 11}, {"Bursa", 6}, {"Antalya", 6}, {"Adana", 4}, {"Konya", 4}, {"Gaziantep", 3},
	{"Kocaeli", 3}, {"Mersin", 3}, {"Eskişehir", 2}, {"Trabzon", 2}, {"Samsun", 2}, {"Diyarbakır", 2}, {"Kayseri", 2}, {"Muğla", 2}}

var ascii = strings.NewReplacer("ç", "c", "ğ", "g", "ı", "i", "ö", "o", "ş", "s", "ü", "u", "Ç", "c", "Ğ", "g", "İ", "i", "Ö", "o", "Ş", "s", "Ü", "u")

func pickCity(r *rand.Rand) string {
	total := 0
	for _, c := range cities {
		total += c.weight
	}
	n := r.Intn(total)
	for _, c := range cities {
		if n < c.weight {
			return c.name
		}
		n -= c.weight
	}
	return cities[0].name
}

// SeedIfEmpty fills `users` with deterministic fake customers (fixed RNG seed).
// Emails use the reserved .test TLD and phone numbers are invented, so nothing
// real can ever be contacted even by mistake.
func (s *Store) SeedIfEmpty(ctx context.Context, n int) (int, error) {
	var existing int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 || n <= 0 {
		return existing, nil
	}
	r := rand.New(rand.NewSource(42))
	now := time.Now()
	rows := make([][]any, 0, n)
	for i := 1; i <= n; i++ {
		first, last := firstNames[r.Intn(len(firstNames))], lastNames[r.Intn(len(lastNames))]
		email := fmt.Sprintf("%s.%s%d@example.test", strings.ToLower(ascii.Replace(first)), strings.ToLower(ascii.Replace(last)), i)
		phone := fmt.Sprintf("+90555%07d", i)
		lang := "tr"
		if r.Float64() < 0.08 {
			lang = "en"
		}
		purchaseCount, totalSpent := 0, 0
		var lastPurchase any
		if r.Float64() >= 0.35 { // 35% have never bought anything
			purchaseCount = 1 + r.Intn(20)
			totalSpent = purchaseCount * (80 + r.Intn(900))
			lastPurchase = now.Add(-time.Duration(r.Intn(365*24)) * time.Hour)
		}
		cartItems := 0
		var lastCart any
		if r.Float64() < 0.55 {
			cartItems = 1 + r.Intn(5)
			lastCart = now.Add(-time.Duration(r.Intn(90*24)) * time.Hour)
		}
		rows = append(rows, []any{first, last, email, phone, pickCity(r), lang, purchaseCount, totalSpent, lastPurchase, cartItems, lastCart,
			r.Float64() < 0.72, r.Float64() < 0.85})
	}
	cols := []string{"first_name", "last_name", "email", "phone", "city", "language", "purchase_count", "total_spent", "last_purchase_at", "cart_items", "last_cart_add_at", "sms_opt_in", "email_opt_in"}
	if _, err := s.pool.CopyFrom(ctx, pgx.Identifier{"users"}, cols, pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("seed users: %w", err)
	}
	return n, nil
}
