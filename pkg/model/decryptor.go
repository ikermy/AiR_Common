package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/crypto"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// MasterKeyProvider умеет получать пользовательский MasterKey от Landing-сервиса.
// bff.Client удовлетворяет этому интерфейсу без изменений (метод GetUserMasterKey уже совпадает).
type MasterKeyProvider interface {
	GetUserMasterKey(ctx context.Context, userID uint32) ([32]byte, error)
}

// ErrMasterKeyUnavailable возвращается когда API-ключ зашифрован MasterKey пользователя,
// но Landing-сервис недоступен или MasterKeyProvider не настроен в роутере.
var ErrMasterKeyUnavailable = errors.New(
	"API-ключ зашифрован MasterKey пользователя: выполните повторную авторизацию через Landing",
)

// masterKeyDecryptingDB оборачивает comdb.Exterior и прозрачно расшифровывает
// $mk$-зашифрованные API-ключи через MasterKeyProvider.
// Все остальные методы делегируются исходному comdb.Exterior.
type masterKeyDecryptingDB struct {
	comdb.Exterior                   // делегирование всех методов
	ctx            context.Context
	mkProvider     MasterKeyProvider
}

// WrapDBWithMasterKeyDecryption возвращает обёртку над db, в которой GetUserAPIKey
// автоматически расшифровывает ключи с префиксом "$mk$" через mkProvider.
//
// Использование: передайте результат вместо исходного db в NewModelRouter или в
// RouterOption WithMasterKeyProvider, который вызывает эту функцию автоматически.
func WrapDBWithMasterKeyDecryption(ctx context.Context, db comdb.Exterior, mkProvider MasterKeyProvider) comdb.Exterior {
	return &masterKeyDecryptingDB{
		Exterior:   db,
		ctx:        ctx,
		mkProvider: mkProvider,
	}
}

// GetUserAPIKey перехватывает стандартный метод и прозрачно расшифровывает $mk$-ключи.
//
//   - Ключ не зашифрован $mk$ (plaintext или $app$) → возвращает как есть (comdb обработает $app$).
//   - Ключ зашифрован $mk$ и mkProvider доступен → расшифровывает через MasterKey.
//   - Ключ зашифрован $mk$ и mkProvider == nil → уведомление "reauth-userkey" + ErrMasterKeyUnavailable.
//   - Ключ зашифрован $mk$ и GetUserMasterKey вернул ошибку (codes.Unavailable = ключ не в кэше,
//     пользователь не входил с момента перезапуска Landing) → уведомление "reauth-userkey" + ErrMasterKeyUnavailable.
func (d *masterKeyDecryptingDB) GetUserAPIKey(userID uint32, provider create.ProviderType) (string, error) {
	key, err := d.Exterior.GetUserAPIKey(userID, provider)
	if err != nil {
		return "", err
	}
	if key == "" || !crypto.IsEncryptedWithMasterKey(key) {
		return key, nil
	}

	// Вспомогательная функция: отправить уведомление о необходимости повторной авторизации.
	sendReauthNotification := func() {
		select {
		case mode.CarpinteroCh <- com.CarpCh{
			Event:  "reauth-userkey",
			UserID: userID,
			Target: provider.String(),
		}:
		default:
			// канал переполнен — уведомление потеряно, но ошибка всё равно вернётся
		}
	}

	// Ключ зашифрован MasterKey пользователя ($mk$) — нужен Landing-сервис
	if d.mkProvider == nil {
		sendReauthNotification()
		return "", ErrMasterKeyUnavailable
	}

	mk, err := d.mkProvider.GetUserMasterKey(d.ctx, userID)
	if err != nil {
		// Наиболее частая причина: codes.Unavailable — ключ не в кэше Landing,
		// пользователь не входил с момента последнего перезапуска сервиса.
		// Другие ошибки (сеть, неверный service key) также блокируют расшифровку.
		// В любом случае пользователю нужно повторно авторизоваться.
		sendReauthNotification()
		return "", fmt.Errorf("%w: %v", ErrMasterKeyUnavailable, err)
	}

	return crypto.DecryptFieldWithMasterKey(mk, key)
}
