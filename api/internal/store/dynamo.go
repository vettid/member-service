package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// DynamoAPI is the part of the DynamoDB client Dynamo uses.
type DynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

// Dynamo is a Store on one DynamoDB table: partition key "pk" (the
// kind), sort key "sk" (the id), the data in "d", a counter in "n" and
// the expiry in "exp" (epoch seconds, the table's TTL attribute). TTL
// deletes lag, so every read also checks exp.
type Dynamo struct {
	Client DynamoAPI
	Table  string
	Now    func() time.Time
}

func (s *Dynamo) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func key(kind, id string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"pk": &types.AttributeValueMemberS{Value: kind}, "sk": &types.AttributeValueMemberS{Value: id}}
}

func epoch(t time.Time) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(t.Unix(), 10)}
}

// live reports whether an item has not expired.
func (s *Dynamo) live(item map[string]types.AttributeValue) bool {
	v, ok := item["exp"].(*types.AttributeValueMemberN)
	if !ok {
		return true
	}
	sec, err := strconv.ParseInt(v.Value, 10, 64)
	return err == nil && s.now().Unix() < sec
}

func data(item map[string]types.AttributeValue) []byte {
	if v, ok := item["d"].(*types.AttributeValueMemberB); ok {
		return v.Value
	}
	return nil
}

// Get implements Store.
func (s *Dynamo) Get(ctx context.Context, kind, id string) ([]byte, bool, error) {
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.Table), Key: key(kind, id), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return nil, false, err
	}
	if out.Item == nil || !s.live(out.Item) {
		return nil, false, nil
	}
	return data(out.Item), true, nil
}

// Put implements Store.
func (s *Dynamo) Put(ctx context.Context, kind, id string, d []byte, exp time.Time, ifAbsent bool) error {
	item := key(kind, id)
	item["d"] = &types.AttributeValueMemberB{Value: d}
	if !exp.IsZero() {
		item["exp"] = epoch(exp)
	}
	in := &dynamodb.PutItemInput{TableName: aws.String(s.Table), Item: item}
	if ifAbsent {
		// Absent, or present but expired and not yet deleted by TTL.
		in.ConditionExpression = aws.String("attribute_not_exists(pk) OR (attribute_exists(exp) AND exp <= :now)")
		in.ExpressionAttributeValues = map[string]types.AttributeValue{":now": epoch(s.now())}
	}
	_, err := s.Client.PutItem(ctx, in)
	var cf *types.ConditionalCheckFailedException
	if errors.As(err, &cf) {
		return ErrExists
	}
	return err
}

// Delete implements Store.
func (s *Dynamo) Delete(ctx context.Context, kind, id string) error {
	_, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{TableName: aws.String(s.Table), Key: key(kind, id)})
	return err
}

// List implements Store.
func (s *Dynamo) List(ctx context.Context, kind string) ([]Item, error) {
	var out []Item
	var start map[string]types.AttributeValue
	for {
		page, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(s.Table),
			KeyConditionExpression:    aws.String("pk = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: kind}},
			ConsistentRead:            aws.Bool(true),
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if !s.live(item) {
				continue
			}
			sk, _ := item["sk"].(*types.AttributeValueMemberS)
			if sk != nil {
				out = append(out, Item{ID: sk.Value, Data: data(item)})
			}
		}
		if len(page.LastEvaluatedKey) == 0 {
			return out, nil
		}
		start = page.LastEvaluatedKey
	}
}

// Incr implements Store: a conditional update while the counter lives;
// otherwise a conditional write starts it again at 1. Two first
// increments at once: one write wins, the other updates after it.
func (s *Dynamo) Incr(ctx context.Context, kind, id string, exp time.Time) (int, error) {
	for range 3 {
		now := epoch(s.now())
		out, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
			TableName:                 aws.String(s.Table),
			Key:                       key(kind, id),
			UpdateExpression:          aws.String("ADD n :one"),
			ConditionExpression:       aws.String("attribute_exists(pk) AND exp > :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":one": &types.AttributeValueMemberN{Value: "1"}, ":now": now},
			ReturnValues:              types.ReturnValueUpdatedNew,
		})
		var cf *types.ConditionalCheckFailedException
		if err == nil {
			v, ok := out.Attributes["n"].(*types.AttributeValueMemberN)
			if !ok {
				return 0, errors.New("store: counter without n")
			}
			return strconv.Atoi(v.Value)
		}
		if !errors.As(err, &cf) {
			return 0, err
		}
		item := key(kind, id)
		item["n"] = &types.AttributeValueMemberN{Value: "1"}
		item["exp"] = epoch(exp)
		_, err = s.Client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(s.Table), Item: item,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR exp <= :now"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":now": now}})
		if err == nil {
			return 1, nil
		}
		if !errors.As(err, &cf) {
			return 0, err
		}
		// Another writer started it first: update again.
	}
	return 0, errors.New("store: counter contended")
}
