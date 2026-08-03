package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestToolServerRunsTheCommandOnItsOwnExecutor proves the runner EXECUTES an exec.request rather
// than relaying it: the command reaches the executor this machine was wired with. What is asserted
// is the ARRIVAL, not the outcome — a fake executor stands in for the machine precisely because the
// machine's behaviour is not what this task adds.
func TestToolServerRunsTheCommandOnItsOwnExecutor(t *testing.T) {
	var seen toolbroker.ShellCommand
	exec := shellFunc(func(_ context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
		seen = cmd
		return toolbroker.ShellResult{ExitCode: 0, Stdout: "ok"}, nil
	})

	out := NewToolServer(exec).Handle(context.Background(), execRequest(t, "exec-1", toolbroker.ShellCommand{
		Argv:          []string{"uname", "-s"},
		WorkspaceRoot: "/workspace",
	}))

	if len(seen.Argv) != 2 || seen.Argv[0] != "uname" || seen.Argv[1] != "-s" {
		t.Fatalf("komut executor'a ulaşmadı: %v", seen.Argv)
	}
	if seen.WorkspaceRoot != "/workspace" {
		t.Fatalf("workspace kökü taşınmadı: %q", seen.WorkspaceRoot)
	}
	if out.Type != ExecResultType {
		t.Fatalf("sonuç mesajının tipi %q, beklenen %q", out.Type, ExecResultType)
	}
	if got := out.Data["exec_id"]; got != "exec-1" {
		t.Fatalf("correlation kimliği geri dönmedi: %v", got)
	}
	var res toolbroker.ShellResult
	mustDecode(t, out.Data["result"], &res)
	if res.Stdout != "ok" {
		t.Fatalf("sonuç geri dönmedi: %+v", res)
	}
}

// TestToolServerReturnsNonZeroExitAsAResultNotAnError pins the rule this tree has already paid for
// once: `false` completes cleanly and answers 1. An executor that reports a non-zero exit has
// SUCCEEDED at running the command, so the wire carries a result — turning it into a refusal wedges
// the run that asked.
func TestToolServerReturnsNonZeroExitAsAResultNotAnError(t *testing.T) {
	exec := shellFunc(func(context.Context, toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
		return toolbroker.ShellResult{ExitCode: 1, Stderr: "nope"}, nil
	})

	out := NewToolServer(exec).Handle(context.Background(), execRequest(t, "exec-1", toolbroker.ShellCommand{Argv: []string{"false"}}))

	if _, refused := out.Data["error"]; refused {
		t.Fatalf("sıfır olmayan çıkış bir reddetmeye çevrildi: %v", out.Data["error"])
	}
	var res toolbroker.ShellResult
	mustDecode(t, out.Data["result"], &res)
	if res.ExitCode != 1 || res.Stderr != "nope" {
		t.Fatalf("ExitCode kayboldu: %+v", res)
	}
}

// TestToolServerAnswersWhenNoExecutorIsWired: a runner that was never given an executor must still
// ANSWER. Silence here is not a degraded mode — it is a tool call the control plane waits on
// forever, which is the failure this tree recorded when every tool Exec error wedged its run.
func TestToolServerAnswersWhenNoExecutorIsWired(t *testing.T) {
	out := NewToolServer(nil).Handle(context.Background(), execRequest(t, "exec-1", toolbroker.ShellCommand{Argv: []string{"uname"}}))

	if out.Type == "" {
		t.Fatal("kablolanmamış runner sessiz kaldı — CP tarafı sonsuza kadar bekler")
	}
	if out.Data["exec_id"] != "exec-1" {
		t.Fatalf("cevap correlation kimliğini taşımıyor, CP onu eşleyemez: %v", out.Data["exec_id"])
	}
	// A refusal, NOT a fabricated exit code: an unwired machine has not run anything, and reporting
	// it as exit 127 would let a misconfiguration read as a command that merely failed.
	if _, refused := out.Data["error"]; !refused {
		t.Fatalf("kablolanmamış runner bir reddetme taşımıyor: %v", out.Data)
	}
	if _, fabricated := out.Data["result"]; fabricated {
		t.Fatalf("hiçbir şey koşmadan bir sonuç uydurdu: %v", out.Data["result"])
	}
}

// TestControllerFrameStillReachesTheEngine is this task's REGRESSION GATE. Before the branch existed
// every inbound message was a controller.frame and went to the engine's stdin; adding a second type
// must not cost that path, because if it does, every run dies. It drives the shipped reader over a
// real websocket — RelayInbound reading a real LeaseSession — so it measures the routing that ships,
// not a re-spelling of it.
func TestControllerFrameStillReachesTheEngine(t *testing.T) {
	session, plane := leasePair(t)
	inbound := make(chan contracts.EngineFrame, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RelayInbound(ctx, session, NewToolServer(nil), inbound, func(string, ...any) {})

	writeMessage(t, plane, contracts.RunnerMessage{
		Protocol: RunnerProtocolV1,
		Type:     "controller.frame",
		Data:     map[string]any{"frame": contracts.EngineFrame{Protocol: "engine.v1", ID: "frame-1", Type: "run.start"}},
	})

	select {
	case frame := <-inbound:
		if frame.ID != "frame-1" || frame.Type != "run.start" {
			t.Fatalf("engine frame'i bozuk ulaştı: %+v", frame)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("controller.frame engine'e ulaşmadı — tel dallanması relay'i kırdı, her run düşer")
	}
}

// TestExecRequestRunsOnTheMachineAndAnswersOverTheWire drives the whole new wire end to end: the
// control plane writes an exec.request, the runner runs it on ITS OWN executor, and an exec.result
// comes back over the same connection carrying the correlation id.
//
// The second assertion is the one that names this task: the request must NOT reach the engine's
// stdin. A tool request that is relayed instead of executed is the exact defect the wire exists to
// remove, and it would be invisible to a test that only checked the answer came back.
func TestExecRequestRunsOnTheMachineAndAnswersOverTheWire(t *testing.T) {
	session, plane := leasePair(t)
	ran := make(chan toolbroker.ShellCommand, 1)
	exec := shellFunc(func(_ context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
		ran <- cmd
		return toolbroker.ShellResult{ExitCode: 0, Stdout: "Darwin\n"}, nil
	})

	inbound := make(chan contracts.EngineFrame, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RelayInbound(ctx, session, NewToolServer(exec), inbound, func(string, ...any) {})

	writeMessage(t, plane, contracts.RunnerMessage{
		Protocol: RunnerProtocolV1,
		Type:     ExecRequestType,
		Data:     ExecRequestData("exec-7", toolbroker.ShellCommand{Argv: []string{"uname", "-s"}}),
	})

	select {
	case cmd := <-ran:
		if len(cmd.Argv) != 2 || cmd.Argv[0] != "uname" {
			t.Fatalf("komut makinenin executor'ına bozuk ulaştı: %v", cmd.Argv)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec.request makinenin executor'ına hiç ulaşmadı")
	}

	answer := readMessage(t, plane)
	if answer.Type != ExecResultType {
		t.Fatalf("cevabın tipi %q, beklenen %q", answer.Type, ExecResultType)
	}
	if answer.Data["exec_id"] != "exec-7" {
		t.Fatalf("cevap correlation kimliğini taşımıyor: %v", answer.Data["exec_id"])
	}
	var res toolbroker.ShellResult
	mustDecode(t, answer.Data["result"], &res)
	if res.Stdout != "Darwin\n" {
		t.Fatalf("makinenin çıktısı tele bozuk çıktı: %+v", res)
	}

	select {
	case frame := <-inbound:
		t.Fatalf("exec.request engine'in stdin'ine enjekte edildi (%+v) — runner onu KOŞTURMALI, relaylememeli", frame)
	default:
	}
}

// shellFunc adapts a function to the real toolbroker.ShellRunner seam, so the tool server under
// test is handed the same interface a machine's executor satisfies.
type shellFunc func(context.Context, toolbroker.ShellCommand) (toolbroker.ShellResult, error)

func (f shellFunc) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	return f(ctx, cmd)
}

func execRequest(t *testing.T, execID string, cmd toolbroker.ShellCommand) contracts.RunnerMessage {
	t.Helper()
	return contracts.RunnerMessage{Protocol: RunnerProtocolV1, Type: ExecRequestType, Data: ExecRequestData(execID, cmd)}
}

// mustDecode re-marshals a data field before decoding it, which is what the wire does to every
// map[string]any it carries: the same helper therefore reads a value built in-process and one that
// arrived as JSON, and a test that only ever saw the in-process shape would miss a field that does
// not survive the round trip.
func mustDecode(t *testing.T, raw any, into any) {
	t.Helper()
	if raw == nil {
		t.Fatal("mesaj beklenen alanı taşımıyor")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// leasePair puts a real websocket between a fake control plane and a real LeaseSession. The session
// holds the DIALED end because that is the end a runner holds: it opens no inbound port.
func leasePair(t *testing.T) (*LeaseSession, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
		<-done
	}))
	t.Cleanup(func() { close(done); server.Close() })

	client, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("lease websocket'i dial edilemedi: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() })

	var plane *websocket.Conn
	select {
	case plane = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("kontrol düzlemi lease websocket'ini hiç kabul etmedi")
	}
	t.Cleanup(func() { _ = plane.CloseNow() })

	return &LeaseSession{
		conn:      client,
		transport: &http.Transport{},
		now:       time.Now,
		lease:     Lease{LeaseID: "lease-1", RunID: "run-1", AttemptID: "attempt-1", Fence: 1},
	}, plane
}

func writeMessage(t *testing.T, conn *websocket.Conn, message contracts.RunnerMessage) {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("mesaj encode edilemedi: %v", err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
		t.Fatalf("mesaj yazılamadı: %v", err)
	}
}

func readMessage(t *testing.T, conn *websocket.Conn) contracts.RunnerMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, payload, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("cevap okunamadı: %v", err)
	}
	var message contracts.RunnerMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatalf("cevap decode edilemedi: %v", err)
	}
	return message
}
