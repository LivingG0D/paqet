package client

import (
	"context"
)

func (c *Client) ticker(ctx context.Context) {
	<-ctx.Done()
}
