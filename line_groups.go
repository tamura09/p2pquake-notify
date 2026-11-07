package main

import (
	"context"
	"log"
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

	var items []map[string]types.AttributeValue
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
	}

	groups, skipped := activeGroupIDs(items)
	if skipped > 0 {
		// 値そのものは出しません。表に壊れた行があることが分かれば足ります。
		log.Printf("line: skipped %d rows with an invalid group_id in %s", skipped, d.table)
	}
	return groups, nil
}

// activeGroupIDs は表の行からグループIDを取り出します。
//
// グループIDの形をしていない行は読み飛ばします。1行のためにスキャン全体を
// エラーにすると、起動時なら本番の報がどのグループにも届かず、起動後なら
// 古い一覧に固定されて招待・退出が反映されなくなります。
func activeGroupIDs(items []map[string]types.AttributeValue) (groups []string, skipped int) {
	groups = []string{}
	seen := map[string]bool{}
	for _, item := range items {
		id, ok := item["group_id"].(*types.AttributeValueMemberS)
		if !ok || !lineGroupPattern.MatchString(id.Value) {
			skipped++
			continue
		}
		if !seen[id.Value] {
			seen[id.Value] = true
			groups = append(groups, id.Value)
		}
	}
	return groups, skipped
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
