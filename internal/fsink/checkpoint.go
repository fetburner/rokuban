package fsink

import (
	"encoding"
	"fmt"
	"hash"
	"os"
)

// Checkpoint は、temp の先頭から書き終えた長さと、その長さまでの SHA-256 状態である。
type Checkpoint struct {
	Offset    int64
	HashState []byte
}

// SaveCheckpoint は f を fsync してから、hasher の状態を offset と組にして save に渡す。
func SaveCheckpoint(f *os.File, offset int64, hasher hash.Hash, save func(Checkpoint) error) error {
	if offset < 0 {
		return fmt.Errorf("checkpoint offset must be non-negative, got %d", offset)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing temp before saving checkpoint: %w", err)
	}
	state, err := MarshalHashState(hasher)
	if err != nil {
		return err
	}
	if err := save(Checkpoint{Offset: offset, HashState: state}); err != nil {
		return fmt.Errorf("saving checkpoint: %w", err)
	}
	return nil
}

// MarshalHashState は hash.Hash のバイナリ状態を返す。
func MarshalHashState(hasher hash.Hash) ([]byte, error) {
	marshaler, ok := hasher.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("hash %T cannot be marshaled", hasher)
	}
	state, err := marshaler.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshaling SHA-256 state: %w", err)
	}
	return state, nil
}

// RestoreHash は SaveCheckpoint が保存した状態を hasher に戻す。戻せなければ error を返す。
func RestoreHash(hasher hash.Hash, state []byte) error {
	unmarshaler, ok := hasher.(encoding.BinaryUnmarshaler)
	if !ok {
		return fmt.Errorf("hash %T cannot be unmarshaled", hasher)
	}
	if len(state) == 0 {
		return fmt.Errorf("SHA-256 state is empty")
	}
	if err := unmarshaler.UnmarshalBinary(state); err != nil {
		return fmt.Errorf("restoring SHA-256 state: %w", err)
	}
	return nil
}
