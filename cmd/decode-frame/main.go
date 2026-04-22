package main
import (
	"fmt"
	"encoding/hex"
	"os"
	"github.com/caligone/openqiara/internal/charmux"
)
func main() {
	for _, h := range os.Args[1:] {
		data, _ := hex.DecodeString(h)
		parsed, err := charmux.DeserializeManagedFrame(data)
		fmt.Printf("=== %s (len=%d) ===\n", h, len(data))
		if err != nil {
			fmt.Println("  err:", err)
			continue
		}
		fmt.Printf("  GWDst=%d GWSrc=%d RFByte=0x%02x Counter=%d Src=%d Flags=0x%04x\n",
			parsed.GWDst, parsed.GWSrc, parsed.RFByte, parsed.Counter, parsed.Src, parsed.Flags)
		fmt.Printf("  AckDst=%d AckCnt=%d WFlags=0x%02x Payload=%x (len=%d)\n",
			parsed.AckDst, parsed.AckCnt, parsed.WFlags, parsed.Payload, len(parsed.Payload))
	}
}
