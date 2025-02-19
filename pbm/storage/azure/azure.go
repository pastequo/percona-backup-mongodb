package azure

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/pkg/errors"

	"github.com/percona/percona-backup-mongodb/pbm/log"
	"github.com/percona/percona-backup-mongodb/pbm/storage"
)

const (
	BlobURL = "https://%s.blob.core.windows.net"

	defaultUploadBuff    = 10 << 20 // 10Mb
	defaultUploadMaxBuff = 5

	defaultRetries = 10

	maxBlocks = 50_000
)

type Conf struct {
	Account     string      `bson:"account" json:"account,omitempty" yaml:"account,omitempty"`
	Container   string      `bson:"container" json:"container,omitempty" yaml:"container,omitempty"`
	Prefix      string      `bson:"prefix" json:"prefix,omitempty" yaml:"prefix,omitempty"`
	Credentials Credentials `bson:"credentials" json:"-" yaml:"credentials"`
}

type Credentials struct {
	Key string `bson:"key" json:"key,omitempty" yaml:"key,omitempty"`
}

type Blob struct {
	opts Conf
	log  *log.Event
	// url  *url.URL
	c *azblob.Client
}

func New(opts Conf, l *log.Event) (b *Blob, err error) {
	b = &Blob{
		opts: opts,
		log:  l,
	}

	b.c, err = b.client()
	if err != nil {
		return nil, errors.Wrap(err, "init container")
	}

	return b, b.ensureContainer()
}

func (*Blob) Type() storage.Type {
	return storage.Azure
}

func (b *Blob) Save(name string, data io.Reader, sizeb int64) error {
	bufsz := defaultUploadBuff
	if sizeb > 0 {
		ps := int(sizeb / maxBlocks * 11 / 10) // add 10% just in case
		if ps > bufsz {
			bufsz = ps
		}
	}

	cc := runtime.NumCPU() / 2
	if cc == 0 {
		cc = 1
	}

	if b.log != nil {
		b.log.Debug("BufferSize is set to %d (~%dMb) | %d", bufsz, bufsz>>20, sizeb)
	}

	_, err := b.c.UploadStream(context.TODO(), b.opts.Container, path.Join(b.opts.Prefix, name), data, &azblob.UploadStreamOptions{
		BlockSize:   int64(bufsz),
		Concurrency: cc,
	})

	return err
}

func (b *Blob) List(prefix, suffix string) ([]storage.FileInfo, error) {
	prfx := path.Join(b.opts.Prefix, prefix)

	if prfx != "" && !strings.HasSuffix(prfx, "/") {
		prfx = prfx + "/"
	}

	pager := b.c.NewListBlobsFlatPager(b.opts.Container, &azblob.ListBlobsFlatOptions{
		Prefix: &prfx,
	})

	var files []storage.FileInfo
	for pager.More() {
		l, err := pager.NextPage(context.TODO())
		if err != nil {
			return nil, errors.Wrap(err, "list segment")
		}

		for _, b := range l.Segment.BlobItems {
			if b.Name == nil {
				return files, errors.Errorf("blob returned nil Name for item %v", b)
			}
			var sz int64
			if b.Properties.ContentLength != nil {
				sz = *b.Properties.ContentLength
			}
			f := *b.Name
			f = strings.TrimPrefix(f, prfx)
			if len(f) == 0 {
				continue
			}
			if f[0] == '/' {
				f = f[1:]
			}

			if strings.HasSuffix(f, suffix) {
				files = append(files, storage.FileInfo{
					Name: f,
					Size: sz,
				})
			}
		}
	}

	return files, nil
}

func (b *Blob) FileStat(name string) (inf storage.FileInfo, err error) {
	p, err := b.c.ServiceClient().NewContainerClient(b.opts.Container).NewBlockBlobClient(path.Join(b.opts.Prefix, name)).GetProperties(context.TODO(), nil)
	if err != nil {
		if isNotFound(err) {
			return inf, storage.ErrNotExist
		}
		return inf, errors.Wrap(err, "get properties")
	}

	inf.Name = name
	if p.ContentLength != nil {
		inf.Size = *p.ContentLength
	}

	if inf.Size == 0 {
		return inf, storage.ErrEmpty
	}

	return inf, nil
}

func (b *Blob) Copy(src, dst string) error {
	to := b.c.ServiceClient().NewContainerClient(b.opts.Container).NewBlockBlobClient(path.Join(b.opts.Prefix, dst))
	from := b.c.ServiceClient().NewContainerClient(b.opts.Container).NewBlockBlobClient(path.Join(b.opts.Prefix, src))
	r, err := to.StartCopyFromURL(context.TODO(), from.BlobClient().URL(), nil)
	if err != nil {
		return errors.Wrap(err, "start copy")
	}

	if r.CopyStatus == nil {
		return errors.New("undefined copy status")
	}
	status := *r.CopyStatus
	for status == blob.CopyStatusTypePending {
		time.Sleep(time.Second * 2)
		p, err := to.GetProperties(context.TODO(), nil)
		if err != nil {
			return errors.Wrap(err, "get copy status")
		}
		if r.CopyStatus == nil {
			return errors.New("undefined copy status")
		}
		status = *p.CopyStatus
	}

	switch status {
	case blob.CopyStatusTypeSuccess:
		return nil

	case blob.CopyStatusTypeAborted:
		return errors.New("copy aborted")
	case blob.CopyStatusTypeFailed:
		return errors.New("copy failed")
	default:
		return errors.Errorf("undefined status")
	}
}

func (b *Blob) SourceReader(name string) (io.ReadCloser, error) {
	o, err := b.c.DownloadStream(context.TODO(), b.opts.Container, path.Join(b.opts.Prefix, name), nil)
	if err != nil {
		return nil, errors.Wrap(err, "download object")
	}

	return o.Body, nil
}

func (b *Blob) Delete(name string) error {
	_, err := b.c.DeleteBlob(context.TODO(), b.opts.Container, path.Join(b.opts.Prefix, name), nil)
	if err != nil {
		if isNotFound(err) {
			return storage.ErrNotExist
		}
		return errors.Wrap(err, "delete object")
	}

	return nil
}

func (b *Blob) ensureContainer() error {
	_, err := b.c.ServiceClient().NewContainerClient(b.opts.Container).GetProperties(context.TODO(), nil)
	// container already exists
	if err == nil {
		return nil
	}
	if stgErr, ok := err.(*azcore.ResponseError); ok {
		if stgErr.StatusCode != http.StatusNotFound {
			return errors.Wrap(err, "check container")
		}
	}

	_, err = b.c.CreateContainer(context.TODO(), b.opts.Container, nil)
	return err
}
func (b *Blob) client() (*azblob.Client, error) {
	cred, err := azblob.NewSharedKeyCredential(b.opts.Account, b.opts.Credentials.Key)
	if err != nil {
		return nil, errors.Wrap(err, "create credentials")
	}

	opts := &azblob.ClientOptions{}
	opts.Retry = policy.RetryOptions{
		MaxRetries: defaultRetries,
	}
	return azblob.NewClientWithSharedKeyCredential(fmt.Sprintf(BlobURL, b.opts.Account), cred, opts)
}

func isNotFound(err error) bool {
	if stgErr, ok := err.(*azcore.ResponseError); ok {
		return stgErr.StatusCode == http.StatusNotFound
	}

	return false
}
