// Preview serves a disposable synthetic installation for visual regression checks.
// Run from the repository root. It never opens the production data directory.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"opendtu-hems-telemetry/internal/auth"
	"opendtu-hems-telemetry/internal/database"
	httpserver "opendtu-hems-telemetry/internal/http"
	"opendtu-hems-telemetry/internal/telemetry"
)

func main() {
	dir, err := os.MkdirTemp("", "hems-dashboard-preview-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	cipher, err := auth.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		log.Fatal(err)
	}
	store, err := database.Open(dir+"/preview.db", cipher)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(15 * time.Minute)
	sender, err := store.CreateSource(ctx, "Demo house · synthetic data", now)
	if err != nil {
		log.Fatal(err)
	}
	if err = store.CreateToken(ctx, sender.ID, now); err != nil {
		log.Fatal(err)
	}
	_, token, err := store.ShowToken(ctx, sender.ID)
	if err != nil {
		log.Fatal(err)
	}
	raw, err := os.ReadFile("internal/dashboard/testdata/rich-hems.json")
	if err != nil {
		log.Fatal(err)
	}
	for i := 0; i < 96; i++ {
		var root map[string]any
		if err = json.Unmarshal(raw, &root); err != nil {
			log.Fatal(err)
		}
		captured := now.Add(time.Duration(i-95) * 15 * time.Minute)
		root["telemetry_sequence"] = i + 1
		root["checkpoint_sequence"] = i + 1
		hems := root["hems"].(map[string]any)
		hems["captured_at_utc"] = captured.Unix()
		normalized := hems["normalized"].(map[string]any)
		day := float64(captured.Hour()) + float64(captured.Minute())/60
		pv := math.Max(0, math.Sin((day-6)/12*math.Pi)) * 4800
		storage := math.Sin(float64(i)/8) * 1200
		evse := 0.
		if i > 70 && i < 79 {
			evse = 2200
		}
		for _, v := range []struct {
			group, key string
			value      float64
		}{{"grid", "power_w", 500 + storage + evse - pv}, {"pv", "ac_power_w", pv}, {"storage", "ac_power_w", storage}, {"storage", "soc_pct", 60 + 20*math.Sin(float64(i)/18)}, {"evse", "power_w", evse}} {
			normalized[v.group].(map[string]any)[v.key] = map[string]any{"quality": "fresh", "value": math.Round(v.value)}
		}
		if i == 40 || i == 41 {
			normalized["evse"].(map[string]any)["power_w"] = map[string]any{"quality": "unknown", "value": nil}
		}
		tariff := hems["tariff"].(map[string]any)["current"].(map[string]any)
		price := 220000 + 90000*math.Sin(float64(i)/10)
		tariff["price_micros_per_kwh"] = math.Round(price)
		tariff["import_price_micros_per_kwh"] = math.Round(price + 100000)
		optimizer := hems["optimizer"].(map[string]any)
		optimizer["current_planned_battery_w"] = math.Round(storage/100) * 100
		for j, v := range optimizer["schedule"].([]any) {
			row := v.(map[string]any)
			row["start"] = now.Add(time.Duration(j) * 15 * time.Minute).Unix()
			row["end"] = now.Add(time.Duration(j+1) * 15 * time.Minute).Unix()
		}
		b, _ := json.Marshal(root)
		m, e := telemetry.Parse(b)
		if e != nil {
			log.Fatal(e)
		}
		if _, _, e = store.Accept(ctx, token, b, m, captured.Add(8*time.Second)); e != nil {
			log.Fatal(e)
		}
	}
	handler, err := httpserver.New(store, "preview", "preview", log.Default())
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Synthetic preview: http://127.0.0.1:8081 · Basic Auth preview / preview")
	log.Fatal(http.ListenAndServe("127.0.0.1:8081", handler))
}
