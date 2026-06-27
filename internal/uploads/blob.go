package uploads

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"

	"github.com/larsakerlund/loft/internal/config"
)

// blobStore writes uploads to an Azure Blob container, authenticated either by a managed identity
// (no key) or by a connection string (e.g. an emulator such as Azurite).
type blobStore struct {
	client    *azblob.Client
	container string
}

// newBlobStore returns a blob-backed store, or (nil, nil) when no blob backend is configured so the
// caller can fall back to a local directory.
func newBlobStore(cfg config.Config) (store, error) {
	switch {
	case cfg.UploadsConnString != "":
		c, err := azblob.NewClientFromConnectionString(cfg.UploadsConnString, nil)
		if err != nil {
			return nil, err
		}
		return &blobStore{client: c, container: cfg.UploadsContainer}, nil
	case cfg.UploadsAccount != "":
		var opts *azidentity.ManagedIdentityCredentialOptions
		if cfg.UAMIClientID != "" {
			opts = &azidentity.ManagedIdentityCredentialOptions{ID: azidentity.ClientID(cfg.UAMIClientID)}
		}
		cred, err := azidentity.NewManagedIdentityCredential(opts)
		if err != nil {
			return nil, err
		}
		c, err := azblob.NewClient("https://"+cfg.UploadsAccount+".blob.core.windows.net", cred, nil)
		if err != nil {
			return nil, err
		}
		return &blobStore{client: c, container: cfg.UploadsContainer}, nil
	default:
		return nil, nil
	}
}

func (b *blobStore) put(ctx context.Context, key, contentType string, body []byte) error {
	// Force a download disposition so an uploaded .html/.svg can't run script in the serving origin
	// (stored XSS) even if the blob is ever served directly, not only behind the ingress's headers.
	attachment := "attachment"
	_, err := b.client.UploadBuffer(ctx, b.container, key, body, &azblob.UploadBufferOptions{
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType, BlobContentDisposition: &attachment},
	})
	return err
}

func (b *blobStore) del(ctx context.Context, key string) error {
	_, err := b.client.DeleteBlob(ctx, b.container, key, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil // idempotent
	}
	return err
}
