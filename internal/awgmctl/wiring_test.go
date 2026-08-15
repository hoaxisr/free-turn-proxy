package awgmctl_test

// Стражи врезок обвязки в оба бинаря.
//
// Тесты гоняют НАСТОЯЩИЕ ft-client и ft-server, а не пакет awgmctl: разъезжается
// молча именно связка «main форка ↔ обвязка», а не сама обвязка. Снятая строка
// logx.OnError в одном из main переживает и сборку, и go vet, и пробу
// --awgm-protocol, и весь ручной стенд второго бинаря; подменённый источник
// argv в ConfigHash не меняет ничего, кроме одного хеша, — а это вечный цикл
// перезапусков после выкладки.

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hoaxisr/awg-manager/awgmproto"
)

// TestClientWiring: клиент отдаёт отпечаток конфигурации, посчитанный из argv
// без argv[0], и доносит до менеджера ошибку, напечатанную своим логгером.
//
// Ошибка берётся настоящая: мёртвый DNS-сервер (-dns-servers 127.0.0.1:1) роняет
// получение TURN-креденшелов независимо от того, есть ли у машины интернет,
// а процесс при этом продолжает жить и повторять попытки.
func TestClientWiring(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "freeturn-client-client-c1.sock")
	args := []string{
		"-peer", "127.0.0.1:9",
		"-links", "https://vk.ru/call/join/x",
		"-listen", "127.0.0.1:0",
		"-dns-mode", "plain",
		"-dns-servers", "127.0.0.1:1",
		"--awgm-control-socket=" + sock,
		"--awgm-log-file=" + filepath.Join(dir, "c.log"),
	}
	start(t, "../../cmd/client", args)

	c := dial(t, sock)
	hello := read(t, c)
	if hello["event"] != "hello" || hello["impl"] != "freeturn-client" || hello["role"] != "client" {
		t.Fatalf("hello: %v", hello)
	}
	if hello["instance"] != "c1" {
		t.Fatalf("instance из имени сокета: %v", hello["instance"])
	}
	if want := awgmproto.ConfigHash(args); hello["config_hash"] != want {
		t.Fatalf("config_hash=%v, менеджер посчитает %s", hello["config_hash"], want)
	}

	ev := readEvent(t, c, awgmproto.EventError)
	msg, _ := ev["message"].(string)
	if msg == "" {
		t.Fatalf("push error без текста: %v", ev)
	}
	t.Logf("push error: %s", msg)

	st := state(t, c)
	if s, _ := st["last_error"].(string); s == "" {
		t.Fatalf("last_error пуст, хотя клиент печатал ошибку")
	}
}

// TestServerWiring: то же для сервера плюс отказ журнала.
//
// Порядок держится на FIFO: сервер блокируется на чтении -clients-file до
// первого писателя, поэтому тест успевает подключиться и снять state, а ошибку
// разбора запускает сам, когда готов. Никаких «успеть за процессом».
func TestServerWiring(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "freeturn-server-server-s1.sock")
	fifo := filepath.Join(dir, "clients.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// Журнал в несуществующем каталоге: отказ обязан доехать до менеджера,
	// потому что без журнала другого канала видимости не остаётся.
	badLog := filepath.Join(dir, "нет-каталога", "s.log")
	args := []string{
		"-listen", "127.0.0.1:0",
		"-connect", "127.0.0.1:9",
		"-clients-file", fifo,
		"--awgm-control-socket=" + sock,
		"--awgm-log-file=" + badLog,
	}
	start(t, "../../cmd/server", args)

	c := dial(t, sock)
	hello := read(t, c)
	if hello["event"] != "hello" || hello["impl"] != "freeturn-server" || hello["role"] != "server" {
		t.Fatalf("hello: %v", hello)
	}
	if hello["instance"] != "s1" {
		t.Fatalf("instance из имени сокета: %v", hello["instance"])
	}
	if want := awgmproto.ConfigHash(args); hello["config_hash"] != want {
		t.Fatalf("config_hash=%v, менеджер посчитает %s", hello["config_hash"], want)
	}

	st := state(t, c)
	if s, _ := st["last_error"].(string); !strings.Contains(s, "журнал") {
		t.Fatalf("отказ журнала не доехал до менеджера: last_error=%q", s)
	}

	// Пишем в FIFO мусор: сервер отвиснет на разборе -clients-file и напечатает
	// ошибку своим логгером.
	done := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			done <- err
			return
		}
		_, err = f.WriteString("не json\n")
		_ = f.Close()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("запись в FIFO: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("сервер не открыл -clients-file на чтение")
	}

	ev := readEvent(t, c, awgmproto.EventError)
	if msg, _ := ev["message"].(string); !strings.Contains(msg, "clients-file") {
		t.Fatalf("push error: %v", ev)
	}
}

// start собирает бинарь пакета и запускает его; процесс убивается по концу теста.
func start(t *testing.T, pkg string, args []string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	build := exec.Command("go", "build", "-o", bin, pkg)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка %s: %v\n%s", pkg, err, out)
	}
	cmd := exec.Command(bin, args...)
	// Вывод процесса — в вывод теста: при отказе журнала это единственное, что
	// от него остаётся.
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Start(); err != nil {
		t.Fatalf("запуск %s: %v", bin, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("процесс: %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// link — соединение менеджера с процессом: сокет плюс его буфер чтения.
type link struct {
	c net.Conn
	r *bufio.Reader
}

func dial(t *testing.T, sock string) *link {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		c, err := net.Dial("unix", sock)
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			// Срок на всё общение: без него мутация «хук снят» не падает, а виснет.
			if err := c.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatalf("срок: %v", err)
			}
			return &link{c: c, r: bufio.NewReader(c)}
		}
		if time.Now().After(deadline) {
			t.Fatalf("процесс не открыл управляющий сокет: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func read(t *testing.T, c *link) map[string]any {
	t.Helper()
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("json %q: %v", line, err)
	}
	if m["v"] != float64(awgmproto.Version) {
		t.Fatalf("версия: %v", m)
	}
	return m
}

// readEvent читает до нужного push, пропуская чужие.
func readEvent(t *testing.T, c *link, event string) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		m := read(t, c)
		if m["event"] == event {
			return m
		}
	}
	t.Fatalf("push %s не пришёл", event)
	return nil
}

func state(t *testing.T, c *link) map[string]any {
	t.Helper()
	if _, err := c.c.Write([]byte(`{"v":1,"id":1,"cmd":"state"}` + "\n")); err != nil {
		t.Fatalf("запрос state: %v", err)
	}
	for i := 0; i < 100; i++ {
		m := read(t, c)
		if m["event"] != nil { // push по дороге — не ответ
			continue
		}
		st, ok := m["state"].(map[string]any)
		if m["ok"] != true || !ok {
			t.Fatalf("ответ на state: %v", m)
		}
		return st
	}
	t.Fatal("ответ на state не пришёл")
	return nil
}
