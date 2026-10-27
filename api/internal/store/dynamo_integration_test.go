//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// newTable creates a table on LocalStack (MS_IT_LOCALSTACK) shaped as
// the CDK makes it: pk and sk strings.
func newTable(t *testing.T, ctx context.Context) *Dynamo {
	t.Helper()
	ep := os.Getenv("MS_IT_LOCALSTACK")
	if ep == "" {
		t.Skip("MS_IT_LOCALSTACK not set")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	c := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(ep) })
	name := fmt.Sprintf("ms-it-%d", time.Now().UnixNano())
	_, err = c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:            aws.String(name),
		BillingMode:          types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}, {AttributeName: aws.String("sk"), AttributeType: types.ScalarAttributeTypeS}},
		KeySchema:            []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}, {AttributeName: aws.String("sk"), KeyType: types.KeyTypeRange}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: aws.String(name)}) })
	return &Dynamo{Client: c, Table: name}
}

// The same semantics as Mem, on DynamoDB: expiry checked on read (TTL
// deletes lag), ifAbsent over an expired record, counters that restart,
// and concurrent first increments that all count.
func TestDynamo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s := newTable(t, ctx)
	now := time.Now()
	s.Now = func() time.Time { return now }
	if err := s.Put(ctx, "k", "a", []byte("1"), now.Add(time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "k", "a", []byte("2"), time.Time{}, true); !errors.Is(err, ErrExists) {
		t.Fatalf("ifAbsent over a live record: %v", err)
	}
	if b, ok, err := s.Get(ctx, "k", "a"); err != nil || !ok || string(b) != "1" {
		t.Fatalf("get %q %v %v", b, ok, err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok, _ := s.Get(ctx, "k", "a"); ok {
		t.Fatal("expired record read")
	}
	if err := s.Put(ctx, "k", "a", []byte("3"), time.Time{}, true); err != nil {
		t.Fatalf("ifAbsent over an expired record: %v", err)
	}
	// An empty record (the webhook replay cache's).
	if err := s.Put(ctx, "k", "b", nil, time.Time{}, true); err != nil {
		t.Fatalf("empty record: %v", err)
	}
	if items, err := s.List(ctx, "k"); err != nil || len(items) != 2 || items[0].ID != "a" || string(items[0].Data) != "3" {
		t.Fatalf("list %+v %v", items, err)
	}
	if err := s.Delete(ctx, "k", "a"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Incr(ctx, "rate", "x", now.Add(time.Hour)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, err := s.Incr(ctx, "rate", "x", now.Add(time.Hour)); err != nil || n != 21 {
		t.Fatalf("after 20 concurrent increments: %d %v", n, err)
	}
	now = now.Add(time.Hour)
	if n, _ := s.Incr(ctx, "rate", "x", now.Add(time.Hour)); n != 1 {
		t.Fatalf("after the window: %d", n)
	}
}
