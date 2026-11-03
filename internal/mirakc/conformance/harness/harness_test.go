//go:build conformance

package harness

import "testing"

// TestMirakcRunArgsImage は StartMirakc が実際に docker へ渡す引数列の末尾（イメージ名）を
// 見る。configuredMirakcImage() 単体のテストでは、StartMirakc 内の配線
// （args = append(args, configuredMirakcImage())）を pin 定数へ直接差し替える変異を
// 検出できない。mirakcRunArgs を経由することで、その配線そのものを検査対象にする。
func TestMirakcRunArgsImage(t *testing.T) {
	lastArg := func(args []string) string { return args[len(args)-1] }

	t.Run("uses the pinned image by default", func(t *testing.T) {
		t.Setenv(mirakcImageEnv, "")
		args := mirakcRunArgs("c", "/tuner", "/config.yml", "/recordings", "")
		if got := lastArg(args); got != MirakcImage {
			t.Fatalf("mirakcRunArgs last arg = %q, want pinned image %q", got, MirakcImage)
		}
	})

	t.Run("uses the environment override", func(t *testing.T) {
		const want = "docker.io/mirakc/mirakc:main-debian"
		t.Setenv(mirakcImageEnv, want)
		args := mirakcRunArgs("c", "/tuner", "/config.yml", "/recordings", "")
		if got := lastArg(args); got != want {
			t.Fatalf("mirakcRunArgs last arg = %q, want %q", got, want)
		}
	})
}
