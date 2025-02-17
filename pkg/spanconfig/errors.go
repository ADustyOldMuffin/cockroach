// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package spanconfig

import (
	"context"

	"github.com/cockroachdb/errors"
	"github.com/gogo/protobuf/proto"
)

// commitTimestampOutOfBoundsError is returned when it's not possible to commit
// an update within the specified time interval.
type commitTimestampOutOfBoundsError struct{}

// NewCommitTimestampOutOfBoundsError constructs a commitTimestampOutOfBoundsError.
func NewCommitTimestampOutOfBoundsError() error {
	return commitTimestampOutOfBoundsError{}
}

// IsCommitTimestampOutOfBoundsError returns whether the given error is the
// commitTimestampOutOfBoundsError kind.
func IsCommitTimestampOutOfBoundsError(err error) bool {
	return errors.Is(err, commitTimestampOutOfBoundsError{})
}

// Error implements the error interface.
func (e commitTimestampOutOfBoundsError) Error() string { return "lease expired" }

func decodeRetryableLeaseExpiredError(context.Context, string, []string, proto.Message) error {
	return NewCommitTimestampOutOfBoundsError()
}

func init() {
	errors.RegisterLeafDecoder(
		errors.GetTypeKey((*commitTimestampOutOfBoundsError)(nil)), decodeRetryableLeaseExpiredError,
	)
}
