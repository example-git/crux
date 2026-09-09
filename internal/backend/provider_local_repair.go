package backend

import (
	"context"
	"errors"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) RepairLocalAuthentication(ctx context.Context, id string, request providerauth.LocalRepairRequest) (response proto.ProviderLocalRepairResponse, err error) {
	response.Request = request
	defer func() {
		if err != nil {
			response.Error = proto.NewProviderLocalRepairError(err)
		}
	}()
	if err = request.Validate(); err != nil {
		return response, err
	}
	if request.WorkspaceID != id {
		return response, errors.New("local repair workspace changed")
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return response, err
	}
	defer done()
	response.Result, err = ws.providerAuth.RepairLocalAuthentication(ctx, request)
	return response, err
}
