package interfaces

import (
	"testing"
)

func TestSaturationScalingConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  SaturationScalingConfig
		wantErr bool
	}{
		{
			name: "valid default config",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.80,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.10,
				QueueSpareTrigger:    3,
			},
			wantErr: false,
		},
		{
			name: "valid custom config",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.75,
				QueueLengthThreshold: 10,
				KvSpareTrigger:       0.15,
				QueueSpareTrigger:    5,
			},
			wantErr: false,
		},
		{
			name: "invalid KvCacheThreshold too high",
			config: SaturationScalingConfig{
				KvCacheThreshold:     1.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvCacheThreshold negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     -0.1,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid QueueLengthThreshold negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: -1,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvSpareTrigger too high",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       1.5,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid KvSpareTrigger negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       -0.1,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "invalid QueueSpareTrigger negative",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.8,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.1,
				QueueSpareTrigger:    -1,
			},
			wantErr: true,
		},
		{
			name: "invalid KvCacheThreshold less than KvSpareTrigger",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.5,
				QueueLengthThreshold: 5,
				KvSpareTrigger:       0.6,
				QueueSpareTrigger:    3,
			},
			wantErr: true,
		},
		{
			name: "edge case: zero values are valid",
			config: SaturationScalingConfig{
				KvCacheThreshold:     0.0,
				QueueLengthThreshold: 0,
				KvSpareTrigger:       0.0,
				QueueSpareTrigger:    0,
			},
			wantErr: false,
		},
		{
			name: "edge case: max values are valid",
			config: SaturationScalingConfig{
				KvCacheThreshold:     1.0,
				QueueLengthThreshold: 1000,
				KvSpareTrigger:       1.0,
				QueueSpareTrigger:    1000,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
