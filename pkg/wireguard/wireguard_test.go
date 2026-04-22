package wireguard

import (
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func TestNewDevice(t *testing.T) {
	privKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	peerKey, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("generate peer key: %v", err)
	}

	dev, err := NewDevice(privKey, peerKey.PublicKey(), "127.0.0.1:51820", 0, TunnelIPClient, nil)
	if err != nil {
		t.Fatalf("new device: %v", err)
	}
	defer dev.Close()

	if dev.Net() == nil {
		t.Error("expected non-nil net")
	}
}
