// Package relay 是遠程操控的中繼協定（ADR-37、D145）：Mac 與手機都主動以
// WebSocket 連出到中繼，中繼按房間轉發不透明的幀。本套件有幀格式、房間
// 號與房間密鑰、Mac 與 App 兩側的客戶端，以及兩端之間的 Noise 加密通道；
// 中繼服務在 relay/server。本套件不導入任何 internal 套件。
package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/flynn/noise"
)

// Version 是幀協定的版本，寫在 Hello 裡。
const Version = 1

// Path 是中繼服務接受 WebSocket 的路徑。
const Path = "/v1"

// MaxPayload 是一幀負載的上限：剛好放下一則 Noise 訊息。
const MaxPayload = noise.MaxMsgLen

// headerLen 是幀頭的長度：一個位元組的種類加四個位元組的連線號。
const headerLen = 5

// MaxFrame 是一幀（含幀頭）的上限，兩側都以它限制讀取。
const MaxFrame = headerLen + MaxPayload

// Kind 是幀的種類。
type Kind byte

const (
	// KindHello：客戶端連上後的第一幀，負載是 Hello 的 JSON。
	KindHello Kind = 1
	// KindReady：中繼 → Mac，已進入房間。
	KindReady Kind = 2
	// KindStatus：中繼 → App，負載一個位元組：1 是 Mac 在線，0 是離線。
	// 在線時 Conn 是 Mac 這次連線的世代號（每次 Mac 連上加一）。
	KindStatus Kind = 3
	// KindOpen：中繼 → Mac，房間裡多了 Conn 這條 App 連線。
	KindOpen Kind = 4
	// KindData：轉發的資料。App 與中繼之間 Conn 是世代號：中繼丟掉 App
	// 送給舊世代的幀（Mac 換了連線，舊的 Noise 會話已作廢）。中繼 → Mac
	// 時 Conn 是來源，Mac → 中繼時 Conn 是去向。
	KindData Kind = 5
	// KindClose：中繼 → Mac，Conn 已離開；Mac → 中繼，斷開 Conn；中繼 →
	// App，Mac 斷開了它（拒絕握手或撤銷設備），之後中繼斷線。
	KindClose Kind = 6
	// KindError：中繼 → 客戶端，負載是代碼（Code），之後中繼斷線。
	KindError Kind = 7
)

func (k Kind) String() string {
	switch k {
	case KindHello:
		return "hello"
	case KindReady:
		return "ready"
	case KindStatus:
		return "status"
	case KindOpen:
		return "open"
	case KindData:
		return "data"
	case KindClose:
		return "close"
	case KindError:
		return "error"
	}
	return fmt.Sprintf("kind(%d)", byte(k))
}

// Frame 是中繼上的一則 WebSocket 二進位訊息：種類、連線號與負載。
type Frame struct {
	Kind    Kind
	Conn    uint32
	Payload []byte
}

// ErrBadFrame 是幀太短、太長或種類不認得。
var ErrBadFrame = errors.New("relay: malformed frame")

// Marshal 把幀編成一則訊息。
func (f Frame) Marshal() []byte {
	b := make([]byte, headerLen+len(f.Payload))
	b[0] = byte(f.Kind)
	binary.BigEndian.PutUint32(b[1:headerLen], f.Conn)
	copy(b[headerLen:], f.Payload)
	return b
}

// ParseFrame 解一則訊息；負載與 b 共用記憶體。
func ParseFrame(b []byte) (Frame, error) {
	if len(b) < headerLen || len(b) > MaxFrame {
		return Frame{}, ErrBadFrame
	}
	k := Kind(b[0])
	if k < KindHello || k > KindError {
		return Frame{}, ErrBadFrame
	}
	return Frame{Kind: k, Conn: binary.BigEndian.Uint32(b[1:headerLen]), Payload: b[headerLen:]}, nil
}

// Reframe 就地改寫一則已由 ParseFrame 解過的訊息的幀頭並回傳它；中繼轉
// 發時不必複製負載。
func Reframe(b []byte, k Kind, conn uint32) []byte {
	b[0] = byte(k)
	binary.BigEndian.PutUint32(b[1:headerLen], conn)
	return b
}

// Role 是連線的一方。
type Role string

const (
	RoleMac Role = "mac"
	RoleApp Role = "app"
)

// Hello 是 KindHello 的負載。App 帶 Room；Mac 只帶 Key，房間號由中繼
// 從密鑰算出（RoomKey.Room），所以只有持有密鑰的才進得了 Mac 那一側。
type Hello struct {
	V    int    `json:"v"`
	Role Role   `json:"role"`
	Room RoomID `json:"room,omitzero"`
	Key  string `json:"key,omitempty"`
}

// RoomID 是房間號：房間密鑰 SHA-256 的前 16 個位元組。App 從二維碼得
// 知它；它只用來分房間，不能用來冒充 Mac。
type RoomID [16]byte

// RoomKey 是 Mac 的房間密鑰。中繼不存它，只在 Mac 連入時算出房間號。
type RoomKey [32]byte

// Room 是這把密鑰的房間號。
func (k RoomKey) Room() RoomID {
	h := sha256.Sum256(k[:])
	return RoomID(h[:16])
}

var b64 = base64.RawURLEncoding

func (r RoomID) String() string { return b64.EncodeToString(r[:]) }

func (r RoomID) MarshalText() ([]byte, error) { return []byte(r.String()), nil }

func (r *RoomID) UnmarshalText(b []byte) error { return decodeFixed(r[:], b, "room id") }

// ParseRoomID 解 RoomID.String 的結果。
func ParseRoomID(s string) (RoomID, error) {
	var r RoomID
	return r, r.UnmarshalText([]byte(s))
}

// String 是 base64url，只用於存進鑰匙串與 Hello，不寫日誌。
func (k RoomKey) String() string { return b64.EncodeToString(k[:]) }

// ParseRoomKey 解 RoomKey.String 的結果。
func ParseRoomKey(s string) (RoomKey, error) {
	var k RoomKey
	return k, decodeFixed(k[:], []byte(s), "room key")
}

func decodeFixed(dst, src []byte, what string) error {
	if b64.EncodedLen(len(dst)) != len(src) {
		return fmt.Errorf("relay: bad %s", what)
	}
	if _, err := b64.Decode(dst, src); err != nil {
		return fmt.Errorf("relay: bad %s", what)
	}
	return nil
}

// NewRoomKey 產生隨機的房間密鑰（D145：遠程第一次打開時，或重置遠程）。
func NewRoomKey() (RoomKey, error) {
	var key RoomKey
	_, err := rand.Read(key[:])
	return key, err
}
