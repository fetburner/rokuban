package store

import (
	"reflect"
	"testing"
)

func TestPlanTransferProgressLease(t *testing.T) {
	uploadStore := "archive"
	emptyUploadStore := ""
	tests := []struct {
		name      string
		input     TransferProgressLeaseInput
		leaseLive bool
		acquired  bool
		want      TransferProgress
	}{
		{
			name: "live lease blocks another owner",
			input: TransferProgressLeaseInput{
				WriterGeneration: 3,
				VerifyingSource:  true,
			},
			leaseLive: true,
			acquired:  false,
		},
		{
			name: "handed off progress resumes and keeps upload state",
			input: TransferProgressLeaseInput{
				WriterGeneration: 4,
				HandedOff:        true,
				Offset:           8192,
				HashState:        []byte("saved-hash"),
				UploadStore:      &uploadStore,
				UploadState:      []byte("saved-upload"),
				VerifyingSource:  true,
			},
			acquired: true,
			want: TransferProgress{
				WriterGeneration:     4,
				PrevWriterGeneration: 4,
				Resumed:              true,
				Offset:               8192,
				HashState:            []byte("saved-hash"),
				UploadStore:          "archive",
				UploadState:          []byte("saved-upload"),
				VerifyingSource:      true,
			},
		},
		{
			name: "handed off progress takes precedence over live lease",
			input: TransferProgressLeaseInput{
				WriterGeneration: 2,
				HandedOff:        true,
				Offset:           5,
			},
			leaseLive: true,
			acquired:  true,
			want: TransferProgress{
				WriterGeneration:     2,
				PrevWriterGeneration: 2,
				Resumed:              true,
				Offset:               5,
			},
		},
		{
			name: "non-null empty upload store retains upload state",
			input: TransferProgressLeaseInput{
				WriterGeneration: 7,
				HandedOff:        true,
				UploadStore:      &emptyUploadStore,
				UploadState:      []byte("saved-upload"),
			},
			acquired: true,
			want: TransferProgress{
				WriterGeneration:     7,
				PrevWriterGeneration: 7,
				Resumed:              true,
				UploadState:          []byte("saved-upload"),
			},
		},
		// Both DB schemas reject this paired-NULL violation; keep it here to check the pure helper's nil branch.
		{
			name: "null upload store drops upload state",
			input: TransferProgressLeaseInput{
				WriterGeneration: 8,
				HandedOff:        true,
				UploadState:      []byte("orphaned-without-store"),
			},
			acquired: true,
			want: TransferProgress{
				WriterGeneration:     8,
				PrevWriterGeneration: 8,
				Resumed:              true,
			},
		},
		{
			name: "expired lease starts a new generation and keeps verification",
			input: TransferProgressLeaseInput{
				WriterGeneration: 9,
				Offset:           100,
				HashState:        []byte("old-hash"),
				UploadStore:      &uploadStore,
				UploadState:      []byte("old-upload"),
				VerifyingSource:  true,
			},
			acquired: true,
			want: TransferProgress{
				WriterGeneration:     10,
				PrevWriterGeneration: 9,
				VerifyingSource:      true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, acquired := PlanTransferProgressLease(tt.input, tt.leaseLive)
			if acquired != tt.acquired {
				t.Fatalf("acquired = %t, want %t", acquired, tt.acquired)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("progress = %+v, want %+v", got, tt.want)
			}
		})
	}
}
