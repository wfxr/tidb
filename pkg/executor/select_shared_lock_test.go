// Copyright 2026 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"testing"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/util/dbterror/exeerrors"
	"github.com/stretchr/testify/require"
	tikvstore "github.com/tikv/client-go/v2/kv"
)

func TestTranslateForShareMilestone1RestrictionErr(t *testing.T) {
	lockCtx := &tikvstore.LockCtx{InShareMode: true}
	rawErr := errors.New(pessimisticSharedLockNeedsPrimaryErr)

	got := translateForShareMilestone1RestrictionErr(lockCtx, rawErr)
	require.True(t, terror.ErrorEqual(got, exeerrors.ErrForShareRequiresPrimaryInMilestone1))
	require.Equal(t, exeerrors.ErrForShareRequiresPrimaryInMilestone1.GenWithStackByArgs().Error(), got.Error())

	cause, ok := errors.Cause(got).(*terror.Error)
	require.True(t, ok, "translated error should remain a terror.Error for SQL write-out")
	require.Equal(t, exeerrors.ErrForShareRequiresPrimaryInMilestone1.GetMsg(), cause.GetMsg())
}
