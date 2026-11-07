package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// lineGroupLookupTimeout はグループの一覧を引く時間の上限。地震の最中に
// テーブルの応答を待ち続けて通知が遅れないよう、短めに切ります。
const lineGroupLookupTimeout = 5 * time.Second

// dynamoLineGroups は line-charge-reminder の Webhook が記録しているグループの表。
// 公式アカウントがグループに招待されると active=true、退出すると false になります。
type dynamoLineGroups struct {
	client *dynamodb.Client
	table  string
}

func (d dynamoLineGroups) activeGroups(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, lineGroupLookupTimeout)
	defer cancel()

	paginator := dynamodb.NewScanPaginator(d.client, &dynamodb.ScanInput{
		TableName:                &d.table,
		ConsistentRead:           aws.Bool(true),
		ProjectionExpression:     aws.String("group_id"),
		FilterExpression:         aws.String("#active = :enabled"),
		ExpressionAttributeNames: map[string]string{"#active": "active"},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":enabled": &types.AttributeValueMemberBOOL{Value: true},
		},
	})

	groups := []string{}
	seen := map[string]bool{}
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			id, ok := item["group_id"].(*types.AttributeValueMemberS)
			if !ok || !lineGroupPattern.MatchString(id.Value) {
				// 値そのものは出しません。表が壊れていることが分かれば足ります。
				return nil, fmt.Errorf("table %s contains an invalid group_id", d.table)
			}
			if !seen[id.Value] {
				seen[id.Value] = true
				groups = append(groups, id.Value)
			}
		}
	}
	return groups, nil
}

func (d dynamoLineGroups) isActive(ctx context.Context, groupID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lineGroupLookupTimeout)
	defer cancel()

	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &d.table,
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			"group_id": &types.AttributeValueMemberS{Value: groupID},
		},
	})
	if err != nil {
		return false, err
	}
	active, ok := out.Item["active"].(*types.AttributeValueMemberBOOL)
	return ok && active.Value, nil
}
