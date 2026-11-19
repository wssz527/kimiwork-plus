package kimi

import (
	"errors"

	"github.com/wssz527/kimiwork-plus/assets/injection"
)

// translationPayload reads the embedded renderer. It is a separate file so the
// controller depends on one function rather than on the asset package directly,
// and so a test can substitute it.
var translationPayload = func() ([]byte, injection.Asset, error) {
	data, asset, err := injection.TranslationRenderer()
	if err != nil {
		return nil, injection.Asset{}, errors.New("injection_payload_unavailable")
	}
	return data, asset, nil
}
