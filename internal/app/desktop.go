package app

// desktop.go：mygo 框架膠水（README 規則 8 允許 app 匯入 mygo）。

import (
	"context"
	"errors"
	"log/slog"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/i18n"

	"github.com/bayuncao/oriel/internal/hub"
	"github.com/bayuncao/oriel/internal/provider"
)

// Notifier 是經 mygo 系統通知發送的 hub.Notifier。
func Notifier(log *slog.Logger) hub.Notifier {
	return notifierFunc(func(_ context.Context, n hub.Notification) error {
		title, body, unknown := render(n, i18n.Locale())
		if unknown != "" && log != nil {
			// 原因的代碼沒有譯文：正文只寫“回合失敗”；記下來好補上（每個代碼都該在 locales 裡有訊息）。
			log.Warn("app: no message for the failure reason", "code", unknown)
		}
		err := mygo.NewNotification(mygo.NotificationOptions{
			ID:    notificationID(n),
			Title: title,
			Body:  body,
		}).Show()
		if err != nil && log != nil {
			log.Debug("app: notification failed", "kind", n.Kind, "err", err)
		}
		return err
	})
}

type notifierFunc func(context.Context, hub.Notification) error

func (f notifierFunc) Notify(ctx context.Context, n hub.Notification) error {
	return f(ctx, n)
}

// Secret 從系統鑰匙圈取值；找不到回傳非 nil 錯誤
// （hub 映射成 hub.secret_missing）。
func Secret(key string) (string, error) {
	b, err := mygo.Secrets.Get(key)
	if err != nil {
		if errors.Is(err, mygo.ErrSecretNotFound) {
			return "", errors.New("secret not found: " + key)
		}
		return "", err
	}
	return string(b), nil
}

// Keychain 是 Provider keys 用的系統鑰匙串（provider.Keychain，D132），由 main 交給 New。
type Keychain struct{}

func (Keychain) Get(name string) ([]byte, error) {
	b, err := mygo.Secrets.Get(name)
	return b, keychainErr(err)
}

func (Keychain) Set(name string, value []byte) error {
	return keychainErr(mygo.Secrets.Set(name, value))
}

func (Keychain) Delete(name string) error { return keychainErr(mygo.Secrets.Delete(name)) }

// keychainErr 把 mygo 的“找不到”換成 provider.ErrNoSecret；其他錯誤（含沒有憑證儲存時的
// errors.ErrUnsupported）原樣返回。
func keychainErr(err error) error {
	if errors.Is(err, mygo.ErrSecretNotFound) {
		return provider.ErrNoSecret
	}
	return err
}

// RenderError 給啟動失敗對話框用。
func RenderError(err error) string { return renderError(err, i18n.Locale()) }
