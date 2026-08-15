package awgmctl

// Обвязка awg-manager для freeturn: управляющий сокет, журнал, отпечатки.
// Пакет общий для cmd/client и cmd/server; кто именно запущен, говорят
// аргументы Setup.

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hoaxisr/awg-manager/awgmproto"
)

const awgmCapPeriod = 30 * time.Second

var (
	awgmOpts    awgmproto.Options
	awgmSrv     *awgmproto.Server
	awgmLog     *awgmproto.Log
	awgmStarted = time.Now()
	awgmState   awgmFTState
)

// awgmFTState — то, что рассказывает о себе freeturn. Ни TUN, ни адреса, ни
// NDMS-интерфейса у него нет: здоровье реле по-прежнему выводится из журнала.
type awgmFTState struct {
	mu           sync.Mutex
	impl         string
	role         string
	instance     string
	configHash   string
	binarySHA256 string
	lastError    string
}

// Setup — ПЕРВАЯ строка main обоих бинарей.
//
// Возвращает argv без awgm-флагов: freeturn разбирает аргументы своим
// config.ParseClient/ParseServer, и неизвестный флаг для него фатален.
// Проба обязана отрабатывать до этого разбора — иначе она потребует полного
// набора обязательных флагов и станет неприменимой.
func Setup(impl, role string) []string {
	full := os.Args[1:]
	rest, opts := awgmproto.SplitArgs(full)
	awgmOpts = opts

	if opts.Protocol {
		if err := awgmproto.PrintProtocol(os.Stdout, awgmproto.ProtocolInfo{
			Impl: impl, Role: role, Commands: []string{awgmproto.CmdState},
		}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}

	awgmproto.IgnoreSIGPIPE()

	if opts.LogFile != "" {
		lg, err := awgmproto.OpenLog(opts.LogFile)
		if err != nil {
			log.Printf("[AWGM] журнал %s: %v", opts.LogFile, err)
		} else {
			awgmLog = lg
			if err := awgmproto.RedirectStdio(lg); err != nil {
				log.Printf("[AWGM] вывод в журнал: %v", err)
			}
			go lg.WatchCap(context.Background(), awgmCapPeriod)
		}
	}

	// Отказ отпечатка не фатален: поле уедет пустым, причина — в last_error и в
	// журнале (§5.2: пустая строка в обязательном поле — «неизвестно»).
	sum, err := awgmproto.BinarySHA256()

	awgmState.mu.Lock()
	if err != nil {
		log.Printf("[AWGM] отпечаток бинаря: %v", err)
		awgmState.lastError = "отпечаток бинаря: " + err.Error()
	}
	awgmState.impl, awgmState.role = impl, role
	awgmState.binarySHA256 = sum
	awgmState.instance = awgmproto.InstanceFromPath(opts.Socket, impl, role)
	// Файловых конфигов в отпечатке нет: -clients-file сервер перечитывает сам,
	// и хеширование превратило бы штатное добавление клиента в allowlist в
	// разрыв живого реле.
	awgmState.configHash = awgmproto.ConfigHash(full)
	awgmState.mu.Unlock()

	os.Args = append(os.Args[:1], rest...)
	awgmStartControl()
	return rest
}

func awgmStartControl() {
	if awgmOpts.Socket == "" {
		return
	}
	st := awgmState.snapshot()
	srv, err := awgmproto.Listen(awgmproto.ServerConfig{
		Path: awgmOpts.Socket, Impl: awgmState.impl, Role: st.Role, Instance: st.Instance,
		Handler: awgmHandler{},
		// Только журнал: OnError зовётся в том числе из Push, и вызов
		// SetError отсюда закрутил бы рекурсию «push не прошёл → пушим».
		OnError: func(err error) { log.Printf("[AWGM] %v", err) },
	})
	if err != nil {
		log.Fatalf("[AWGM] управляющий сокет: %v", err)
	}
	awgmSrv = srv
	go srv.Serve()
}

func (s *awgmFTState) snapshot() awgmproto.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return awgmproto.State{
		Role: s.role, Instance: s.instance, PID: os.Getpid(),
		ConfigHash: s.configHash, BinarySHA256: s.binarySHA256,
		UptimeS: int64(time.Since(awgmStarted).Seconds()), LastError: s.lastError,
	}
}

// SetError запоминает последнюю ошибку и будит менеджера. Зовётся из хука
// logx.OnError — единственной общей точки печати ошибок у форка.
func SetError(message string, fatal bool) {
	awgmState.mu.Lock()
	awgmState.lastError = message
	awgmState.mu.Unlock()
	awgmPush(awgmproto.Event{Event: awgmproto.EventError, Message: message, Fatal: fatal})
}

// PushExit — best effort: смерть процесса менеджер и так увидит по
// закрытию соединения.
//
// Через Server.PushExit, а не через общий Push: exit стоит на пути выключения, и
// обычный срок записи задержал бы его на пять секунд при недоступном менеджере.
func PushExit(code int) {
	if awgmSrv != nil {
		awgmSrv.PushExit(code)
	}
}

func awgmPush(ev awgmproto.Event) {
	if awgmSrv != nil {
		awgmSrv.Push(ev)
	}
}

// awgmHandler — команды протокола для freeturn. TUN у него нет.
type awgmHandler struct{}

func (awgmHandler) State() awgmproto.State           { return awgmState.snapshot() }
func (awgmHandler) AttachTun(string, *os.File) error { return awgmproto.ErrNotSupported }
func (awgmHandler) DetachTun() error                 { return awgmproto.ErrNotSupported }
