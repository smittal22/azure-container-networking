package nodenetworkconfig

import (
	"context"
	"testing"

	"github.com/Azure/azure-container-networking/cns"
	"github.com/Azure/azure-container-networking/cns/logger"
	cnstypes "github.com/Azure/azure-container-networking/cns/types"
	"github.com/Azure/azure-container-networking/crd/nodenetworkconfig/api/v1alpha"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type cnsClientState struct {
	req *cns.CreateNetworkContainerRequest
	nnc *v1alpha.NodeNetworkConfig
}

type mockCNSClient struct {
	state            cnsClientState
	createOrUpdateNC func(*cns.CreateNetworkContainerRequest) cnstypes.ResponseCode
	update           func(*v1alpha.NodeNetworkConfig) error
}

func (m *mockCNSClient) CreateOrUpdateNetworkContainerInternal(req *cns.CreateNetworkContainerRequest) cnstypes.ResponseCode {
	m.state.req = req
	return m.createOrUpdateNC(req)
}

func (m *mockCNSClient) Update(nnc *v1alpha.NodeNetworkConfig) error {
	m.state.nnc = nnc
	return m.update(nnc)
}

type mockNCGetter struct {
	get func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error)
}

func (m *mockNCGetter) Get(ctx context.Context, key types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
	return m.get(ctx, key)
}

func TestReconcile(t *testing.T) {
	logger.InitLogger("", 0, 0, "")
	tests := []struct {
		name               string
		in                 reconcile.Request
		ncGetter           mockNCGetter
		cnsClient          mockCNSClient
		nodeIP             string
		want               reconcile.Result
		wantCNSClientState cnsClientState
		wantErr            bool
	}{
		{
			name: "unknown get err",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return nil, errors.New("")
				},
			},
			wantErr: true,
		},
		{
			name: "not found",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return nil, apierrors.NewNotFound(schema.GroupResource{}, "")
				},
			},
			wantErr: false,
		},
		{
			name: "no NCs",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return &v1alpha.NodeNetworkConfig{}, nil
				},
			},
			wantErr: false,
		},
		{
			name: "invalid NCs",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return &v1alpha.NodeNetworkConfig{
						Status: invalidStatusMultiNC,
					}, nil
				},
			},
			wantErr: true,
		},
		{
			name: "err in CreateOrUpdateNC",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return &v1alpha.NodeNetworkConfig{
						Status: validSwiftStatus,
					}, nil
				},
			},
			cnsClient: mockCNSClient{
				createOrUpdateNC: func(*cns.CreateNetworkContainerRequest) cnstypes.ResponseCode {
					return cnstypes.UnexpectedError
				},
			},
			wantErr: true,
			wantCNSClientState: cnsClientState{
				req: validSwiftRequest,
			},
		},
		{
			name: "success",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return &v1alpha.NodeNetworkConfig{
						Status: validSwiftStatus,
						Spec: v1alpha.NodeNetworkConfigSpec{
							RequestedIPCount: 1,
						},
					}, nil
				},
			},
			cnsClient: mockCNSClient{
				createOrUpdateNC: func(*cns.CreateNetworkContainerRequest) cnstypes.ResponseCode {
					return cnstypes.Success
				},
				update: func(*v1alpha.NodeNetworkConfig) error {
					return nil
				},
			},
			wantErr: false,
			wantCNSClientState: cnsClientState{
				req: validSwiftRequest,
				nnc: &v1alpha.NodeNetworkConfig{
					Status: validSwiftStatus,
					Spec: v1alpha.NodeNetworkConfigSpec{
						RequestedIPCount: 1,
					},
				},
			},
		},
		{
			name: "node IP mismatch",
			ncGetter: mockNCGetter{
				get: func(context.Context, types.NamespacedName) (*v1alpha.NodeNetworkConfig, error) {
					return &v1alpha.NodeNetworkConfig{
						Status: validSwiftStatus,
						Spec: v1alpha.NodeNetworkConfigSpec{
							RequestedIPCount: 1,
						},
					}, nil
				},
			},
			cnsClient: mockCNSClient{
				createOrUpdateNC: func(*cns.CreateNetworkContainerRequest) cnstypes.ResponseCode {
					return cnstypes.Success
				},
				update: func(*v1alpha.NodeNetworkConfig) error {
					return nil
				},
			},
			nodeIP:             "192.168.1.5", // nodeIP in above NNC status is 10.1.0.5
			wantErr:            false,
			wantCNSClientState: cnsClientState{}, // state should be empty since we should skip this NC
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			r := NewReconciler(&tt.cnsClient, &tt.cnsClient, tt.nodeIP)
			r.nnccli = &tt.ncGetter
			got, err := r.Reconcile(context.Background(), tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantCNSClientState, tt.cnsClient.state)
		})
	}
}
