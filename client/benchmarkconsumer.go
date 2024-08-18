package client

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
)

type BenchmarkConsumer struct {
	objectStorageClient      *minio.Client
	bucketName               string
	nextObjectBufferPosition int
	objects                  []*benchmarkObjectInDownload
	downloadTasks            chan downloadTask

	CollectMetrics        bool
	CollectMetricsLock    sync.RWMutex
	returnLatencies       chan []time.Duration
	returnBytesDownloaded chan uint64
	returnFilesDownloaded chan int
	bytesConsumed         uint64
	filesConsumed         int
	logger                *zap.Logger
	done                  chan struct{}
}

type downloadTask struct {
	name           string
	bufferPosition int
}

type benchmarkObjectInDownload struct {
	size             int64
	name             string
	readLock         sync.Mutex
	changeObjectLock sync.Mutex
}

type MinioMetrics struct {
	FilesDownloaded    int
	BytesDownloaded    uint64
	FilesConsumed      int
	BytesConsumed      uint64
	FirstByteLatencies []time.Duration
}

func NewBenchmarkConsumer(bucketName string, endpoint, accessKey, secretAccessKey string, logger *zap.Logger) (*BenchmarkConsumer, error) {
	objectStorageClient, err := MinioClient(endpoint, accessKey, secretAccessKey, Concurrency)
	if err != nil {
		return nil, fmt.Errorf("failed to create object storage client: %v", err)
	}
	benchmarkConsumer := &BenchmarkConsumer{
		done:                  make(chan struct{}),
		objectStorageClient:   objectStorageClient,
		bucketName:            bucketName,
		downloadTasks:         make(chan downloadTask),
		objects:               make([]*benchmarkObjectInDownload, Concurrency),
		returnLatencies:       make(chan []time.Duration),
		returnBytesDownloaded: make(chan uint64),
		returnFilesDownloaded: make(chan int),
		logger:                logger,
	}
	for i := range benchmarkConsumer.objects {
		object := &benchmarkObjectInDownload{}
		object.readLock.Lock()
		benchmarkConsumer.objects[i] = object
	}
	go benchmarkConsumer.findDownloadableObjectsBenchmark()
	for range Concurrency {
		go benchmarkConsumer.downloadObjectsBenchmark()
	}
	return benchmarkConsumer, nil
}

func (c *BenchmarkConsumer) findDownloadableObjectsBenchmark() {
	objectNames := make([]string, 0)
	for objectInfo := range c.objectStorageClient.ListObjects(context.Background(), c.bucketName, minio.ListObjectsOptions{}) {
		if objectInfo.Err != nil {
			c.logger.Panic("Error looking for existing objects in bucket", zap.Error(objectInfo.Err), zap.String("bucketName", c.bucketName))
		}
		objectNames = append(objectNames, objectInfo.Key)
	}
	c.logger.Info("Learned about minio objects", zap.String("bucketName", c.bucketName), zap.Int("numberObjects", len(objectNames)))

	bufferPosition := 0
	for _, name := range objectNames {
		object := c.objects[bufferPosition]
		object.changeObjectLock.Lock()
		c.downloadTasks <- downloadTask{
			name:           name,
			bufferPosition: bufferPosition,
		}
		bufferPosition = (bufferPosition + 1) % Concurrency
	}
	numObjects := len(objectNames)
	index := 0
	for {
		select {
		case <-c.done:
			c.logger.Info("Stop feeding downloadable objects", zap.String("bucketName", c.bucketName))
		default:
			if index == 0 {
				c.logger.Info("Starting new circle")
			}
			current := objectNames[index]
			objectInDownload := c.objects[bufferPosition]
			objectInDownload.changeObjectLock.Lock()
			c.downloadTasks <- downloadTask{
				name:           current,
				bufferPosition: bufferPosition,
			}
			bufferPosition = (bufferPosition + 1) % Concurrency
			index = (index + 1) % numObjects
		}
	}
}

type firstByteRecorder struct {
	t *time.Time
	r io.Reader
}

func (f *firstByteRecorder) Read(p []byte) (n int, err error) {
	if f.t != nil || len(p) == 0 {
		return f.r.Read(p)
	}
	// Read a single byte.
	n, err = f.r.Read(p[:1])
	if n > 0 {
		t := time.Now()
		f.t = &t
	}
	return n, err
}

func (c *BenchmarkConsumer) downloadObjectsBenchmark() {
	latencies := make([]time.Duration, 0)
	filesDownloaded := 0
	var bytesDownloaded uint64
	for {
		select {
		case <-c.done:
			c.logger.Info("Stop downloading Objects", zap.String("bucketName", c.bucketName))
			c.returnLatencies <- latencies
			c.returnFilesDownloaded <- filesDownloaded
			c.returnBytesDownloaded <- bytesDownloaded
			c.logger.Info("Download objects routine returned metrics", zap.Int("numLatencies", len(latencies)), zap.Int("files", filesDownloaded), zap.Uint64("bytes", bytesDownloaded))
			return
		case downloadTask := <-c.downloadTasks:
			benchmarkObjectInDownload := c.objects[downloadTask.bufferPosition]
			benchmarkObjectInDownload.name = downloadTask.name
			c.logger.Info("Starting download of new object", zap.String("objectName", downloadTask.name), zap.Int("bufferPosition", downloadTask.bufferPosition))
			object, err := c.objectStorageClient.GetObject(context.TODO(), c.bucketName, downloadTask.name, minio.GetObjectOptions{})
			if err != nil {
				c.logger.Error("Failed to download object from s3", zap.Error(err), zap.String("objectName", downloadTask.name))
				return
			}
			stats, err := object.Stat()
			if err != nil {
				c.logger.Error("Failed to get object stats", zap.Error(err), zap.String("objectName", downloadTask.name))
				return
			}
			if stats.Size == 0 {
				c.logger.Info("Downloaded object of size 0", zap.String("objectName", downloadTask.name))
				c.CollectMetricsLock.RLock()
				if c.CollectMetrics {
					filesDownloaded++
				}
				c.CollectMetricsLock.RUnlock()
				benchmarkObjectInDownload.size = 0
				benchmarkObjectInDownload.readLock.Unlock()
				continue
			}
			fbr := &firstByteRecorder{
				r: object,
			}
			start := time.Now()
			n, err := io.Copy(io.Discard, fbr)
			if err != nil {
				c.logger.Error("Failed to copy object data", zap.Error(err))
			}
			if n != stats.Size {
				c.logger.Error("Read less bytes than expected", zap.Int64("expected", stats.Size), zap.Int64("read", n))
			}
			c.CollectMetricsLock.RLock()
			if c.CollectMetrics {
				c.logger.Info("Collecting metrics")
				latencies = append(latencies, fbr.t.Sub(start))
				filesDownloaded++
				bytesDownloaded += uint64(stats.Size)
			}
			c.CollectMetricsLock.RUnlock()
			benchmarkObjectInDownload.size = n
			benchmarkObjectInDownload.readLock.Unlock()
			c.logger.Info("Unlocked read lock", zap.Int("bufferPosition", downloadTask.bufferPosition), zap.String("name", downloadTask.name))
		}
	}
}

func (c *BenchmarkConsumer) NextObject() error {
	benchmarkObject := c.objects[c.nextObjectBufferPosition]
	c.logger.Info("Waiting to read object", zap.Int("bufferPosition", c.nextObjectBufferPosition))
	benchmarkObject.readLock.Lock()
	c.logger.Info("Read object", zap.String("name", benchmarkObject.name))
	c.CollectMetricsLock.RLock()
	if c.CollectMetrics {
		c.bytesConsumed += uint64(benchmarkObject.size)
		c.filesConsumed++
	}
	c.CollectMetricsLock.RUnlock()
	benchmarkObject.changeObjectLock.Unlock()
	return nil
}

func (c *BenchmarkConsumer) Close() error {
	c.logger.Info("Finished consume call", zap.String("partitionName", c.bucketName))
	close(c.done)
	return nil
}

func (c *BenchmarkConsumer) Metrics() MinioMetrics {
	latencies := make([]time.Duration, 0)
	for range Concurrency {
		latencies = append(latencies, <-c.returnLatencies...)
	}
	filesDownloaded := 0
	var bytesDownloaded uint64
	for range Concurrency {
		filesDownloaded += <-c.returnFilesDownloaded
		bytesDownloaded += <-c.returnBytesDownloaded
	}
	return MinioMetrics{
		FirstByteLatencies: latencies,
		BytesDownloaded:    bytesDownloaded,
		FilesDownloaded:    filesDownloaded,
		BytesConsumed:      c.bytesConsumed,
		FilesConsumed:      c.filesConsumed,
	}
}
