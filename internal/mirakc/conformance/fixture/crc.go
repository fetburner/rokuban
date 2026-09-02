//go:build conformance

package fixture

// crc32MPEG は MPEG-2 Systems / DVB PSI が使う CRC32（poly 0x04C11DB7, init 0xFFFFFFFF,
// 反転なし）。ビット直列の教科書実装で足りる速度要件（フィクスチャのセクションは数十バイト）。
func crc32MPEG(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc ^= uint32(b) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
