package _139

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

func parallelTestData(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func parallelTestFile(b []byte) model.File {
	return bytes.NewReader(b)
}

// 状态寄存器的取值固定下来，一旦 crypto/sha256 的序列化布局发生变化就会失败。
func TestPlanParallelPartsGolden(t *testing.T) {
	data := parallelTestData(320)
	fullHash, partInfos, err := planParallelParts(parallelTestFile(data), int64(len(data)), 128)
	if err != nil {
		t.Fatal(err)
	}

	const wantHash = "7f2fcab03c5e118e07abe79b7db86af8a7e78d2e312b68e58e1d262c7de84f39"
	if fullHash != wantHash {
		t.Errorf("full hash = %s, want %s", fullHash, wantHash)
	}
	if len(partInfos) != 3 {
		t.Fatalf("got %d parts, want 3", len(partInfos))
	}

	wantSizes := []int64{128, 128, 64}
	wantOffsets := []int64{0, 128, 256}
	for i, p := range partInfos {
		if p.PartNumber != int64(i+1) {
			t.Errorf("part %d: number = %d", i, p.PartNumber)
		}
		if p.PartSize != wantSizes[i] {
			t.Errorf("part %d: size = %d, want %d", i, p.PartSize, wantSizes[i])
		}
		if p.offset != wantOffsets[i] {
			t.Errorf("part %d: offset = %d, want %d", i, p.offset, wantOffsets[i])
		}
	}

	// 第一个分片从初始向量开始，不带上下文
	if partInfos[0].ParallelHashCtx != nil {
		t.Error("part 1 should carry no hash context")
	}

	wantRegs := [][]uint32{
		{0x593253ad, 0xfb4cc018, 0xbe611395, 0x485e47c1, 0x5a5b271d, 0xfb8da14f, 0xe8f77fb4, 0xd05eacbc},
		{0xedba51c8, 0xd14f2c6e, 0x30929803, 0x50ce3ed0, 0xc0270451, 0x7ebc882b, 0x7a17accc, 0x2ed273ad},
	}
	for i, want := range wantRegs {
		ctx := partInfos[i+1].ParallelHashCtx
		if ctx == nil {
			t.Fatalf("part %d has no hash context", i+2)
		}
		if ctx.PartOffset != wantOffsets[i+1] {
			t.Errorf("part %d: partOffset = %d, want %d", i+2, ctx.PartOffset, wantOffsets[i+1])
		}
		if !reflect.DeepEqual(ctx.H, want) {
			t.Errorf("part %d: h = %#v, want %#v", i+2, ctx.H, want)
		}
	}
}

// 每个分片的上下文都应当等于该分片起点之前所有字节的哈希中间状态。
func TestPlanParallelPartsMatchesPrefixState(t *testing.T) {
	data := parallelTestData(5000)
	_, partInfos, err := planParallelParts(parallelTestFile(data), int64(len(data)), 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range partInfos {
		if p.ParallelHashCtx == nil {
			continue
		}
		h := sha256.New()
		h.Write(data[:p.ParallelHashCtx.PartOffset])
		want, hashed, err := sha256Midstate(h)
		if err != nil {
			t.Fatal(err)
		}
		if hashed != p.ParallelHashCtx.PartOffset {
			t.Fatalf("part %d: hashed %d bytes, want %d", p.PartNumber, hashed, p.ParallelHashCtx.PartOffset)
		}
		if !reflect.DeepEqual(p.ParallelHashCtx.H, want) {
			t.Errorf("part %d: h = %#v, want %#v", p.PartNumber, p.ParallelHashCtx.H, want)
		}
	}
}

// 分片大小不是 64 的整数倍时，未满一组的字节仍留在缓冲区里，只导出寄存器会丢数据。
func TestPlanParallelPartsRejectsUnalignedPartSize(t *testing.T) {
	data := parallelTestData(300)
	if _, _, err := planParallelParts(parallelTestFile(data), int64(len(data)), 100); err == nil {
		t.Fatal("expected an error for a part size that is not a multiple of 64")
	}
}

func TestPlanParallelPartsEmptyFile(t *testing.T) {
	fullHash, partInfos, err := planParallelParts(parallelTestFile(nil), 0, 128)
	if err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256(nil)
	if fullHash != hex.EncodeToString(empty[:]) {
		t.Errorf("full hash = %s, want %s", fullHash, hex.EncodeToString(empty[:]))
	}
	if len(partInfos) != 1 {
		t.Fatalf("got %d parts, want 1", len(partInfos))
	}
	if partInfos[0].PartSize != 0 || partInfos[0].ParallelHashCtx != nil {
		t.Errorf("unexpected part %+v", partInfos[0])
	}
}
