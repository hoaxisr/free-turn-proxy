package captcha

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/samosvalishe/free-turn-proxy/internal/provider/vk/internal/browserprofile"
)

func testPersona(plat browserprofile.Platform) browserprofile.Profile {
	return browserprofile.For(plat, browserprofile.Identity{Seed: "telemetry-test"})
}

func testGesture(p browserprofile.Profile) gesture {
	l := layoutFor(p)
	target := l.sliderTarget(3, 8)
	return gesture{
		from:   approachFrom(p, l, target),
		to:     target,
		move:   1500 * time.Millisecond,
		settle: 400 * time.Millisecond,
	}
}

// Виджет всегда сериализует все семь ключей аккумулятора: пустой массив - валидный
// ответ браузера, отсутствующий ключ - невозможное состояние.
func TestAnalyticsAlwaysSendsAllKeys(t *testing.T) {
	want := []string{"accelerometer", "gyroscope", "motion", "cursor", "taps", "connectionRtt", "connectionDownlink"}
	for _, plat := range []browserprofile.Platform{browserprofile.Desktop, browserprofile.Mobile} {
		p := testPersona(plat)
		fields := buildAnalytics(p, defaultSensorConfig(), testGesture(p), 4*time.Second).fields()
		if len(fields) != len(want) {
			t.Fatalf("%s: %d fields, want %d", plat, len(fields), len(want))
		}
		for i, kv := range fields {
			if kv[0] != want[i] {
				t.Fatalf("%s: field %d = %q, want %q", plat, i, kv[0], want[i])
			}
			if kv[1] == "" {
				t.Fatalf("%s: field %q empty, want at least []", plat, kv[0])
			}
		}
	}
}

// Телеметрия не может содержать данных, которых персона физически не соберёт.
func TestAnalyticsRespectsPersonaCapabilities(t *testing.T) {
	cases := []struct {
		plat        browserprofile.Platform
		wantAccel   bool
		wantPointer string
	}{
		{browserprofile.Desktop, false, "cursor"},
		{browserprofile.Mobile, true, "taps"},
	}
	for _, tc := range cases {
		p := testPersona(tc.plat)
		data := buildAnalytics(p, defaultSensorConfig(), testGesture(p), 4*time.Second)

		if len(data.connRtt) == 0 {
			t.Fatalf("%s: NetworkInformation отсутствует", tc.plat)
		}
		if len(data.connRtt) != len(data.connDownlink) {
			t.Fatalf("%s: rtt %d != downlink %d", tc.plat, len(data.connRtt), len(data.connDownlink))
		}
		if got := len(data.accelerometer) > 0; got != tc.wantAccel {
			t.Fatalf("%s: accelerometer present = %v", tc.plat, got)
		}
		if len(data.gyroscope) != 0 || len(data.motion) != 0 {
			t.Fatalf("%s: web-персона не может дать gyroscope/motion", tc.plat)
		}
		if tc.wantPointer == "cursor" && (len(data.cursor) == 0 || len(data.taps) != 0) {
			t.Fatalf("%s: мышь обязана писать cursor и не писать taps", tc.plat)
		}
		if tc.wantPointer == "taps" && (len(data.taps) == 0 || len(data.cursor) != 0) {
			t.Fatalf("%s: тач обязан писать taps и не писать cursor", tc.plat)
		}
	}
}

// Число сэмплов задаёт таймер виджета: сколько тиков уместилось в реальное время
// сессии, столько и точек. Мышь видна с первого тика, палец - только с касания.
func TestAnalyticsSampleCountFollowsElapsed(t *testing.T) {
	cfg := sensorConfig{delay: 100 * time.Millisecond, cursor: true, taps: true, accelerometer: true}
	elapsed := 5 * time.Second
	ticks := int(elapsed / cfg.delay)

	desktop := testPersona(browserprofile.Desktop)
	data := buildAnalytics(desktop, cfg, testGesture(desktop), elapsed)
	if len(data.cursor) != ticks || len(data.connRtt) != ticks {
		t.Fatalf("desktop: cursor %d, conn %d, want %d", len(data.cursor), len(data.connRtt), ticks)
	}

	mobile := testPersona(browserprofile.Mobile)
	g := testGesture(mobile)
	data = buildAnalytics(mobile, cfg, g, elapsed)
	gestureTicks := int((g.move + g.settle) / cfg.delay)
	if len(data.taps) > gestureTicks+1 || len(data.taps) == 0 {
		t.Fatalf("mobile: taps %d, want <= %d (только время касания)", len(data.taps), gestureTicks+1)
	}
	if len(data.connRtt) != ticks {
		t.Fatalf("mobile: conn %d, want %d", len(data.connRtt), ticks)
	}
}

// Сервер сам говорит, что слушает: неслушаемый сенсор остаётся пустым.
func TestAnalyticsHonoursSensorList(t *testing.T) {
	raw := map[string]any{"response": map[string]any{
		"sensors_delay":       float64(250),
		"bridge_sensors_list": []any{"cursor"},
	}}
	cfg := parseSensorConfig(raw)
	if cfg.delay != 250*time.Millisecond || !cfg.cursor || cfg.taps || cfg.accelerometer {
		t.Fatalf("sensor config = %+v", cfg)
	}

	p := testPersona(browserprofile.Mobile)
	data := buildAnalytics(p, cfg, testGesture(p), 3*time.Second)
	if len(data.taps) != 0 || len(data.accelerometer) != 0 {
		t.Fatalf("taps %d, accelerometer %d: сервер их не слушает", len(data.taps), len(data.accelerometer))
	}
}

// Сессия короче одного тика: массивы пустые, но ключи на месте.
func TestAnalyticsWithoutTicksIsEmpty(t *testing.T) {
	p := testPersona(browserprofile.Mobile)
	cfg := sensorConfig{delay: 2 * time.Second, cursor: true, taps: true, accelerometer: true}
	data := buildAnalytics(p, cfg, testGesture(p), 500*time.Millisecond)
	for _, kv := range data.fields() {
		if kv[1] != "[]" {
			t.Fatalf("field %q = %s, want []", kv[0], kv[1])
		}
	}
}

func TestParseSensorConfigFallsBackToDefaults(t *testing.T) {
	cfg := parseSensorConfig(map[string]any{"error": map[string]any{"error_code": float64(5)}})
	if cfg != defaultSensorConfig() {
		t.Fatalf("sensor config = %+v, want defaults", cfg)
	}
	clamped := parseSensorConfig(map[string]any{"response": map[string]any{"sensors_delay": float64(1)}})
	if clamped.delay != sensorDelayFloor {
		t.Fatalf("delay = %s, want clamp to %s", clamped.delay, sensorDelayFloor)
	}
}

// Координаты указателя обязаны лежать внутри окна, заявленного в componentDone.
func TestPointerStaysInsideViewport(t *testing.T) {
	for _, plat := range []browserprofile.Platform{browserprofile.Desktop, browserprofile.Mobile} {
		p := testPersona(plat)
		data := buildAnalytics(p, defaultSensorConfig(), testGesture(p), 4*time.Second)
		for _, pt := range append(append([]point{}, data.cursor...), data.taps...) {
			if pt.X < 0 || pt.Y < 0 || pt.X > p.Viewport.W || pt.Y > p.Viewport.H {
				t.Fatalf("%s: точка %+v вне окна %+v", plat, pt, p.Viewport)
			}
		}
	}
}

// Chrome огрубляет NetworkInformation: rtt кратен 25 мс, downlink - шагу 0.05 и
// не превышает 10 Мбит/с.
func TestConnectionSamplesQuantized(t *testing.T) {
	rtt, downlink := sampleConnection(false, 40)
	for i := range rtt {
		if rtt[i] <= 0 || rtt[i]%25 != 0 {
			t.Fatalf("rtt[%d] = %d, want positive multiple of 25", i, rtt[i])
		}
		if downlink[i] <= 0 || downlink[i] > 10 {
			t.Fatalf("downlink[%d] = %v, want (0, 10]", i, downlink[i])
		}
		if steps := downlink[i] / 0.05; steps-float64(int(steps+0.5)) > 1e-9 {
			t.Fatalf("downlink[%d] = %v, want multiple of 0.05", i, downlink[i])
		}
	}
}

func TestAccelerometerSamplesAreGravity(t *testing.T) {
	samples := sampleAccelerometer(12)
	if len(samples) != 12 {
		t.Fatalf("samples = %d, want 12", len(samples))
	}
	for _, s := range samples {
		if mag := s.X*s.X + s.Y*s.Y + s.Z*s.Z; mag < 64 || mag > 144 {
			t.Fatalf("magnitude^2 = %.1f, not gravity-like", mag)
		}
	}
}

func TestJSONArrayEmptyIsBrackets(t *testing.T) {
	if got := jsonArray[point](nil); got != "[]" {
		t.Fatalf("jsonArray(nil) = %q, want []", got)
	}
	var pts []point
	if err := json.Unmarshal([]byte(jsonArray([]point{{X: 1, Y: 2}})), &pts); err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].X != 1 || pts[0].Y != 2 {
		t.Fatalf("round-trip = %+v", pts)
	}
}
