/*
Copyright The Velero Contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package block

import (
	"context"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/vmware-tanzu/velero/pkg/repository/udmrepo"
	udmrepomocks "github.com/vmware-tanzu/velero/pkg/repository/udmrepo/mocks"
)

func TestLoadObjectFromSnapshot(t *testing.T) {
	testCases := []struct {
		name           string
		snapshot       *udmrepo.Snapshot
		setupMocks     func(repo *udmrepomocks.BackupRepo)
		expectedErrStr string
		expectedID     udmrepo.ID
	}{
		{
			name:           "nil snapshot",
			snapshot:       nil,
			expectedErrStr: "snapshot is empty",
		},
		{
			name: "ReadMetadata error",
			snapshot: &udmrepo.Snapshot{
				RootObject: udmrepo.ObjectMetadata{ID: "root-obj"},
			},
			setupMocks: func(repo *udmrepomocks.BackupRepo) {
				repo.On("ReadMetadata", mock.Anything, udmrepo.ID("root-obj")).
					Return(nil, errors.New("read error"))
			},
			expectedErrStr: "error reading snapshot metadata",
		},
		{
			name: "unexpected number of subobjects (0)",
			snapshot: &udmrepo.Snapshot{
				RootObject: udmrepo.ObjectMetadata{ID: "root-obj"},
			},
			setupMocks: func(repo *udmrepomocks.BackupRepo) {
				repo.On("ReadMetadata", mock.Anything, udmrepo.ID("root-obj")).
					Return(&udmrepo.Metadata{SubObjects: []udmrepo.ObjectMetadata{}}, nil)
			},
			expectedErrStr: "unexpected number of bdev object",
		},
		{
			name: "unexpected number of subobjects (2)",
			snapshot: &udmrepo.Snapshot{
				RootObject: udmrepo.ObjectMetadata{ID: "root-obj"},
			},
			setupMocks: func(repo *udmrepomocks.BackupRepo) {
				repo.On("ReadMetadata", mock.Anything, udmrepo.ID("root-obj")).
					Return(&udmrepo.Metadata{SubObjects: []udmrepo.ObjectMetadata{{ID: "obj-1"}, {ID: "obj-2"}}}, nil)
			},
			expectedErrStr: "unexpected number of bdev object",
		},
		{
			name: "success",
			snapshot: &udmrepo.Snapshot{
				RootObject: udmrepo.ObjectMetadata{ID: "root-obj"},
			},
			setupMocks: func(repo *udmrepomocks.BackupRepo) {
				repo.On("ReadMetadata", mock.Anything, udmrepo.ID("root-obj")).
					Return(&udmrepo.Metadata{SubObjects: []udmrepo.ObjectMetadata{{ID: "bdev-obj"}}}, nil)
			},
			expectedID: "bdev-obj",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mockRepo := udmrepomocks.NewBackupRepo(t)

			if tc.setupMocks != nil {
				tc.setupMocks(mockRepo)
			}

			id, err := loadObjectFromSnapshot(ctx, mockRepo, tc.snapshot)

			if tc.expectedErrStr != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tc.expectedErrStr)
				assert.Empty(t, id)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedID, id)
			}
		})
	}
}
