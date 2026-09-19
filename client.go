package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpTimeout leaves room inside commandBudget for config loading and for the
// reply to be written before the host's lifecycle deadline expires.
const httpTimeout = 3500 * time.Millisecond

// maxResponseBytes bounds what the agent may return. The agent caps fastfetch
// output itself; this guards against a hostile or broken endpoint.
const maxResponseBytes = 256 * 1024

type fetchRequest struct {
	Args []string `json:"args"`
}

type fetchResponse struct {
	Output    string `json:"output"`
	Host      string `json:"host"`
	TookMS    int64  `json:"took_ms"`
	Truncated bool   `json:"truncated"`
}

type healthResponse struct {
	OK        bool   `json:"ok"`
	Host      string `json:"host"`
	Fastfetch string `json:"fastfetch"`
	UptimeS   int64  `json:"uptime_s"`
}

type agentError struct {
	Error string `json:"error"`
}

// callFailure is a request that produced no result. offline marks the failures
// a stored snapshot can stand in for: the machine is asleep, off the network,
// or momentarily unable to run fastfetch. A rejected token, a bad address or a
// missing fastfetch is not one — answering those with old output would leave
// the operator hunting a problem the module had already named.
type callFailure struct {
	message string
	offline bool
}

var client = &http.Client{Timeout: httpTimeout}

// call performs one agent request and decodes its result. It returns nil on
// success, or a failure carrying a ready-to-send user-facing message.
func call(ctx context.Context, cfg *config, method, path string, body []byte, out any) *callFailure {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, cfg.URL+path, reader)
	if err != nil {
		return &callFailure{message: "⚠️ Некорректный адрес агента: " + cfg.URL}
	}
	request.Header.Set("Authorization", "Bearer "+cfg.Token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return &callFailure{
				message: fmt.Sprintf("⏱ %s не ответил за %s.", cfg.displayName(""), httpTimeout),
				offline: true,
			}
		}
		return &callFailure{
			message: fmt.Sprintf(
				"🔌 %s недоступен (%s). Проверь, что он не спит, в сети и lavis-fetchd запущен.",
				cfg.displayName(""), cfg.URL,
			),
			offline: true,
		}
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return &callFailure{message: "⚠️ Оборвался ответ агента.", offline: true}
	}
	if response.StatusCode != http.StatusOK {
		return agentFailure(response.StatusCode, payload)
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &callFailure{message: "⚠️ Непонятный ответ агента."}
	}
	return nil
}

func agentFailure(status int, payload []byte) *callFailure {
	var body agentError
	detail := ""
	if err := json.Unmarshal(payload, &body); err == nil {
		detail = body.Error
	}
	switch status {
	case http.StatusUnauthorized:
		return &callFailure{message: "🔒 Агент отклонил токен. Сверь token в конфиге модуля и --token-file агента."}
	case http.StatusTooManyRequests:
		return &callFailure{message: "⏳ Агент занят другим запросом, повтори.", offline: true}
	case http.StatusGatewayTimeout:
		return &callFailure{message: "⏱ Fastfetch на удалённой машине не уложился в таймаут агента.", offline: true}
	case http.StatusServiceUnavailable:
		return &callFailure{message: "⚠️ На удалённой машине нет fastfetch."}
	}
	// A gateway error means fastfetch itself failed on the other machine just
	// now, which the next attempt may well survive.
	offline := status == http.StatusBadGateway
	if detail == "" {
		return &callFailure{message: fmt.Sprintf("⚠️ Агент вернул HTTP %d.", status), offline: offline}
	}
	return &callFailure{message: "⚠️ " + clip(detail), offline: offline}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
