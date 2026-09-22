package secret

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// testKey 是一把固定的 32 字节测试密钥。用固定值而不是每次随机：失败要能复现，
// 而且下面有几条用例要的就是「同一把密钥、两次封同一个明文」。
func testKey() *Keyring {
	keyring, err := New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		panic(err)
	}
	return keyring
}

func TestSealOpenRoundTrip(t *testing.T) {
	keyring := testKey()
	cases := []struct {
		name, slot, plaintext string
		kind                  Kind
	}{
		{name: "单行密钥", slot: "signKey", kind: KindText, plaintext: "test-sign-key-9f3a"},
		{name: "多行 PEM", slot: "merchantPrivateKey", kind: KindPEM,
			plaintext: "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\nkq9w==\n-----END PRIVATE KEY-----\n"},
		{name: "中文与特殊字符", slot: "appKey", kind: KindText, plaintext: `a"b\c&d=e 中文 🎫`},
		{name: "恰好一个字符", slot: "secret", kind: KindText, plaintext: "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope, err := keyring.Seal(tc.slot, tc.kind, tc.plaintext)
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			if envelope.Kind != tc.kind {
				t.Errorf("Kind = %q, want %q", envelope.Kind, tc.kind)
			}
			got, err := keyring.Open(tc.slot, envelope)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if got != tc.plaintext {
				t.Errorf("解出来的明文与原文不同")
			}
		})
	}
}

// TestCiphertextDoesNotContainPlaintext 是一条粗筛，防的是「哪天有人把实现改成
// 编码而不是加密」——那种改动下所有往返测试都还是绿的。
func TestCiphertextDoesNotContainPlaintext(t *testing.T) {
	const plaintext = "super-secret-signing-key"
	envelope, err := testKey().Seal("signKey", KindText, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(envelope.Ciphertext, plaintext) {
		t.Fatal("密文里出现了明文")
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), plaintext) {
		t.Fatal("序列化后的信封里出现了明文")
	}
}

// TestNonceIsFreshPerSeal：同一个明文封两次必须是两个不同的信封。
//
// 这条不是洁癖：GCM 在同一把密钥下复用 nonce，攻击者能直接恢复出异或后的明文，
// 并伪造出能通过认证的密文。随机 nonce 的长度是 12 字节，重复概率可以忽略，
// 这条用例真正防的是「有人为了省一次 rand.Read 把 nonce 固定下来」。
func TestNonceIsFreshPerSeal(t *testing.T) {
	keyring := testKey()
	first, err := keyring.Seal("signKey", KindText, "same-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := keyring.Seal("signKey", KindText, "same-value")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first.Nonce == second.Nonce {
		t.Fatal("两次加密用了同一个 nonce")
	}
	if first.Ciphertext == second.Ciphertext {
		t.Fatal("同一明文两次加密得到了相同密文")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	keyring := testKey()
	envelope, err := keyring.Seal("signKey", KindText, "test-sign-key-9f3a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// 改密文里的一个字节（重新 base64 编码，保证改动确实落在字节上而不是编码上）。
	sealed, err := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	if err != nil {
		t.Fatalf("解码密文: %v", err)
	}
	sealed[0] ^= 0x01
	tampered := envelope
	tampered.Ciphertext = base64.StdEncoding.EncodeToString(sealed)

	if _, err := keyring.Open("signKey", tampered); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("改了密文仍然解得开，得到 %v", err)
	}
}

// TestOpenBindsSlotAndKind 是 AAD 那两条线：密文搬到别的槽、或者 kind 被改掉，
// 都必须解不开。少了这条，能改库的人就能拿一个槽的密钥去顶另一个槽的用。
func TestOpenBindsSlotAndKind(t *testing.T) {
	keyring := testKey()
	envelope, err := keyring.Seal("signKey", KindText, "test-sign-key-9f3a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := keyring.Open("platformCert", envelope); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("把 signKey 的密文放进 platformCert 槽仍然解得开，得到 %v", err)
	}

	changedKind := envelope
	changedKind.Kind = KindPEM
	if _, err := keyring.Open("signKey", changedKind); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("把 kind 改成 pem 之后仍然解得开，得到 %v", err)
	}
}

func TestOpenWithAnotherMasterKeyFails(t *testing.T) {
	envelope, err := testKey().Seal("signKey", KindText, "test-sign-key-9f3a")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	other, err := New([]byte("ffffffffffffffffffffffffffffffff"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := other.Open("signKey", envelope); !errors.Is(err, ErrDecryptFailed) {
		t.Errorf("换了一把主密钥仍然解得开，得到 %v", err)
	}
}

func TestOpenRejectsMalformedEnvelope(t *testing.T) {
	keyring := testKey()
	cases := []struct {
		name     string
		envelope Envelope
		want     error
	}{
		{name: "空信封", envelope: Envelope{}, want: ErrKindUnknown},
		{name: "kind 不认识", envelope: Envelope{Kind: "binary"}, want: ErrKindUnknown},
		{name: "nonce 不是 base64", envelope: Envelope{Kind: KindText, Nonce: "!!!", Ciphertext: "AAAA"}, want: ErrCiphertextMalformed},
		{name: "nonce 长度不对", envelope: Envelope{Kind: KindText, Nonce: base64.StdEncoding.EncodeToString([]byte("short")), Ciphertext: "AAAA"}, want: ErrCiphertextMalformed},
		{name: "密文不是 base64", envelope: Envelope{Kind: KindText, Nonce: base64.StdEncoding.EncodeToString(make([]byte, nonceSize)), Ciphertext: "!!!"}, want: ErrCiphertextMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := keyring.Open("signKey", tc.envelope); !errors.Is(err, tc.want) {
				t.Errorf("得到 %v，期望 %v", err, tc.want)
			}
		})
	}
}

func TestSealRefusesEmptyPlaintext(t *testing.T) {
	// 「槽在、值是空的」与「没有这个槽」在解析那一层意思相反（见包注释），
	// 所以宁可在这里拒绝，也不要在库里留下一个含义模糊的状态。
	if _, err := testKey().Seal("signKey", KindText, ""); !errors.Is(err, ErrPlaintextEmpty) {
		t.Errorf("空明文应当被拒绝，得到 %v", err)
	}
}

func TestSealRejectsUnknownKind(t *testing.T) {
	if _, err := testKey().Seal("signKey", "binary", "x"); !errors.Is(err, ErrKindUnknown) {
		t.Errorf("未知 kind 应当被拒绝，得到 %v", err)
	}
}

func TestNilKeyringIsRefusedNotSilentlySkipped(t *testing.T) {
	// nil Keyring 是「没配主密钥」。它必须**报错**而不是返回空串放行 ——
	// 空串在验签那一层是「验不了签就拒绝」，但一个静默的 nil 很容易被写成
	// 「解不开就当没有」，那等于把加密存储变成了一层摆设。
	var keyring *Keyring
	if _, err := keyring.Seal("signKey", KindText, "x"); !errors.Is(err, ErrMasterKeyMissing) {
		t.Errorf("Seal: 得到 %v，期望 ErrMasterKeyMissing", err)
	}
	if _, err := keyring.Open("signKey", Envelope{Kind: KindText}); !errors.Is(err, ErrMasterKeyMissing) {
		t.Errorf("Open: 得到 %v，期望 ErrMasterKeyMissing", err)
	}
}

// —— Parse ——

func TestParseAcceptsBase64AndHex(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	cases := []struct {
		name, encoded string
	}{
		{name: "base64 带填充", encoded: base64.StdEncoding.EncodeToString(raw)},
		{name: "base64 不带填充", encoded: base64.RawStdEncoding.EncodeToString(raw)},
		{name: "hex", encoded: hex.EncodeToString(raw)},
		{name: "前后有空白", encoded: "  " + base64.StdEncoding.EncodeToString(raw) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyring, err := Parse(tc.encoded)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// 解出来的必须就是那 32 个字节，而不是别的什么。
			envelope, err := keyring.Seal("signKey", KindText, "x")
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			direct, err := New(raw)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := direct.Open("signKey", envelope); err != nil {
				t.Errorf("Parse 解出的密钥与原始 32 字节不是同一把: %v", err)
			}
		})
	}
}

func TestParseRejectsBadMasterKeys(t *testing.T) {
	cases := []struct {
		name, encoded string
		want          error
	}{
		{name: "空", encoded: "", want: ErrMasterKeyMissing},
		{name: "只有空白", encoded: "   ", want: ErrMasterKeyMissing},
		// 32 个 ASCII 字符看着像密钥，熵却远低于 256 位 —— 认它就等于把
		// 「必须随机」降级成一句建议。
		{name: "32 字符短语", encoded: "0123456789abcdef0123456789abcdef", want: ErrMasterKeyInvalid},
		{name: "31 字节", encoded: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcde")), want: ErrMasterKeyInvalid},
		{name: "33 字节", encoded: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdefg")), want: ErrMasterKeyInvalid},
		{name: "占位文本", encoded: "replace-me", want: ErrMasterKeyInvalid},
		{name: "63 位 hex", encoded: strings.Repeat("a", 63), want: ErrMasterKeyInvalid},
		{name: "64 位但不是 hex", encoded: strings.Repeat("z", 64), want: ErrMasterKeyInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.encoded); !errors.Is(err, tc.want) {
				t.Errorf("得到 %v，期望 %v", err, tc.want)
			}
		})
	}
}

// TestErrorsNeverCarryTheMasterKeyOrPlaintext：错误会进日志、会被包进上层错误串，
// 明文或密钥跟进去就等于把它们写到了另一个地方。
func TestErrorsNeverCarryTheMasterKeyOrPlaintext(t *testing.T) {
	const plaintext = "super-secret-signing-key"
	const masterKey = "0123456789abcdef0123456789abcdef"

	keyring, err := Parse(base64.StdEncoding.EncodeToString([]byte(masterKey)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	envelope, err := keyring.Seal("signKey", KindText, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	wrong, err := Parse(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	_, openErr := wrong.Open("signKey", envelope)
	if openErr == nil {
		t.Fatal("换了一把密钥不该解得开")
	}

	for _, err := range []error{openErr, ErrMasterKeyInvalid, ErrDecryptFailed, ErrPlaintextEmpty} {
		message := err.Error()
		if strings.Contains(message, plaintext) {
			t.Errorf("错误串里出现了明文: %v", err)
		}
		if strings.Contains(message, masterKey) {
			t.Errorf("错误串里出现了主密钥: %v", err)
		}
	}
}

// —— Slots ——

func TestSlotsNamesAreSortedAndComplete(t *testing.T) {
	keyring := testKey()
	slots := Slots{}
	for _, slot := range []string{"platformCert", "signKey", "apiV3Key", "merchantPrivateKey"} {
		envelope, err := keyring.Seal(slot, KindText, "value-of-"+slot)
		if err != nil {
			t.Fatalf("Seal(%s): %v", slot, err)
		}
		slots[slot] = envelope
	}

	got := slots.Names()
	want := []string{"apiV3Key", "merchantPrivateKey", "platformCert", "signKey"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
	var emptySlots Slots
	if empty := emptySlots.Names(); len(empty) != 0 {
		t.Errorf("空 slots 的 Names() = %v，期望空", empty)
	}
}

func TestSlotsSurviveJSONRoundTrip(t *testing.T) {
	// 这一列进的是 JSONB，所以信封必须能原样过一遍 json。
	keyring := testKey()
	slots := Slots{}
	envelope, err := keyring.Seal("merchantPrivateKey", KindPEM, "-----BEGIN PRIVATE KEY-----\nAAAA\n")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	slots["merchantPrivateKey"] = envelope

	encoded, err := json.Marshal(slots)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decoded := Slots{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := keyring.Open("merchantPrivateKey", decoded["merchantPrivateKey"])
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != "-----BEGIN PRIVATE KEY-----\nAAAA\n" {
		t.Errorf("PEM 过了一遍 JSON 之后变了")
	}
}

func TestParseKind(t *testing.T) {
	if kind, err := ParseKind(" PEM "); err != nil || kind != KindPEM {
		t.Errorf("ParseKind(\" PEM \") = %q, %v", kind, err)
	}
	if _, err := ParseKind("binary"); !errors.Is(err, ErrKindUnknown) {
		t.Errorf("得到 %v，期望 ErrKindUnknown", err)
	}
}
