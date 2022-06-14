// Code generated by mockery. DO NOT EDIT.

package mocks

import (
	bytes "github.com/tendermint/tendermint/libs/bytes"

	context "context"

	coretypes "github.com/tendermint/tendermint/rpc/coretypes"

	mock "github.com/stretchr/testify/mock"

	testing "testing"
)

// SignClient is an autogenerated mock type for the SignClient type
type SignClient struct {
	mock.Mock
}

// Block provides a mock function with given fields: ctx, height
func (_m *SignClient) Block(ctx context.Context, height *int64) (*coretypes.ResultBlock, error) {
	ret := _m.Called(ctx, height)

	var r0 *coretypes.ResultBlock
	if rf, ok := ret.Get(0).(func(context.Context, *int64) *coretypes.ResultBlock); ok {
		r0 = rf(ctx, height)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultBlock)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *int64) error); ok {
		r1 = rf(ctx, height)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// BlockByHash provides a mock function with given fields: ctx, hash
func (_m *SignClient) BlockByHash(ctx context.Context, hash bytes.HexBytes) (*coretypes.ResultBlock, error) {
	ret := _m.Called(ctx, hash)

	var r0 *coretypes.ResultBlock
	if rf, ok := ret.Get(0).(func(context.Context, bytes.HexBytes) *coretypes.ResultBlock); ok {
		r0 = rf(ctx, hash)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultBlock)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, bytes.HexBytes) error); ok {
		r1 = rf(ctx, hash)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// BlockResults provides a mock function with given fields: ctx, height
func (_m *SignClient) BlockResults(ctx context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	ret := _m.Called(ctx, height)

	var r0 *coretypes.ResultBlockResults
	if rf, ok := ret.Get(0).(func(context.Context, *int64) *coretypes.ResultBlockResults); ok {
		r0 = rf(ctx, height)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultBlockResults)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *int64) error); ok {
		r1 = rf(ctx, height)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// BlockSearch provides a mock function with given fields: ctx, query, page, perPage, orderBy
func (_m *SignClient) BlockSearch(ctx context.Context, query string, page *int, perPage *int, orderBy string) (*coretypes.ResultBlockSearch, error) {
	ret := _m.Called(ctx, query, page, perPage, orderBy)

	var r0 *coretypes.ResultBlockSearch
	if rf, ok := ret.Get(0).(func(context.Context, string, *int, *int, string) *coretypes.ResultBlockSearch); ok {
		r0 = rf(ctx, query, page, perPage, orderBy)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultBlockSearch)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, string, *int, *int, string) error); ok {
		r1 = rf(ctx, query, page, perPage, orderBy)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Commit provides a mock function with given fields: ctx, height
func (_m *SignClient) Commit(ctx context.Context, height *int64) (*coretypes.ResultCommit, error) {
	ret := _m.Called(ctx, height)

	var r0 *coretypes.ResultCommit
	if rf, ok := ret.Get(0).(func(context.Context, *int64) *coretypes.ResultCommit); ok {
		r0 = rf(ctx, height)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultCommit)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *int64) error); ok {
		r1 = rf(ctx, height)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Header provides a mock function with given fields: ctx, height
func (_m *SignClient) Header(ctx context.Context, height *int64) (*coretypes.ResultHeader, error) {
	ret := _m.Called(ctx, height)

	var r0 *coretypes.ResultHeader
	if rf, ok := ret.Get(0).(func(context.Context, *int64) *coretypes.ResultHeader); ok {
		r0 = rf(ctx, height)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultHeader)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *int64) error); ok {
		r1 = rf(ctx, height)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// HeaderByHash provides a mock function with given fields: ctx, hash
func (_m *SignClient) HeaderByHash(ctx context.Context, hash bytes.HexBytes) (*coretypes.ResultHeader, error) {
	ret := _m.Called(ctx, hash)

	var r0 *coretypes.ResultHeader
	if rf, ok := ret.Get(0).(func(context.Context, bytes.HexBytes) *coretypes.ResultHeader); ok {
		r0 = rf(ctx, hash)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultHeader)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, bytes.HexBytes) error); ok {
		r1 = rf(ctx, hash)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Tx provides a mock function with given fields: ctx, hash, prove
func (_m *SignClient) Tx(ctx context.Context, hash bytes.HexBytes, prove bool) (*coretypes.ResultTx, error) {
	ret := _m.Called(ctx, hash, prove)

	var r0 *coretypes.ResultTx
	if rf, ok := ret.Get(0).(func(context.Context, bytes.HexBytes, bool) *coretypes.ResultTx); ok {
		r0 = rf(ctx, hash, prove)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultTx)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, bytes.HexBytes, bool) error); ok {
		r1 = rf(ctx, hash, prove)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// TxSearch provides a mock function with given fields: ctx, query, prove, page, perPage, orderBy
func (_m *SignClient) TxSearch(ctx context.Context, query string, prove bool, page *int, perPage *int, orderBy string) (*coretypes.ResultTxSearch, error) {
	ret := _m.Called(ctx, query, prove, page, perPage, orderBy)

	var r0 *coretypes.ResultTxSearch
	if rf, ok := ret.Get(0).(func(context.Context, string, bool, *int, *int, string) *coretypes.ResultTxSearch); ok {
		r0 = rf(ctx, query, prove, page, perPage, orderBy)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultTxSearch)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, string, bool, *int, *int, string) error); ok {
		r1 = rf(ctx, query, prove, page, perPage, orderBy)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Validators provides a mock function with given fields: ctx, height, page, perPage, requestQuorumInfo
func (_m *SignClient) Validators(ctx context.Context, height *int64, page *int, perPage *int, requestQuorumInfo *bool) (*coretypes.ResultValidators, error) {
	ret := _m.Called(ctx, height, page, perPage, requestQuorumInfo)

	var r0 *coretypes.ResultValidators
	if rf, ok := ret.Get(0).(func(context.Context, *int64, *int, *int, *bool) *coretypes.ResultValidators); ok {
		r0 = rf(ctx, height, page, perPage, requestQuorumInfo)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*coretypes.ResultValidators)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(context.Context, *int64, *int, *int, *bool) error); ok {
		r1 = rf(ctx, height, page, perPage, requestQuorumInfo)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// NewSignClient creates a new instance of SignClient. It also registers the testing.TB interface on the mock and a cleanup function to assert the mocks expectations.
func NewSignClient(t testing.TB) *SignClient {
	mock := &SignClient{}
	mock.Mock.Test(t)

	t.Cleanup(func() { mock.AssertExpectations(t) })

	return mock
}