//                           _       _
// __      _____  __ ___   ___  __ _| |_ ___
// \ \ /\ / / _ \/ _` \ \ / / |/ _` | __/ _ \
//  \ V  V /  __/ (_| |\ V /| | (_| | ||  __/
//   \_/\_/ \___|\__,_| \_/ |_|\__,_|\__\___|
//
//  Copyright © 2016 - 2024 Weaviate B.V. All rights reserved.
//
//  CONTACT: hello@weaviate.io
//

package vectorizer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus/hooks/test"

	"github.com/stretchr/testify/require"
	"github.com/weaviate/weaviate/entities/models"
)

func TestBatch(t *testing.T) {
	client := &fakeBatchClient{}
	cfg := &fakeClassConfig{vectorizePropertyName: false, classConfig: map[string]interface{}{"vectorizeClassName": false}}
	logger, _ := test.NewNullLogger()
	cases := []struct {
		name       string
		objects    []*models.Object
		skip       []bool
		wantErrors map[int]error
		deadline   time.Duration
	}{
		{name: "skip all", objects: []*models.Object{{Class: "Car"}}, skip: []bool{true}},
		{name: "skip first", objects: []*models.Object{{Class: "Car"}, {Class: "Car", Properties: map[string]interface{}{"test": "test"}}}, skip: []bool{true, false}},
		{name: "one object errors", objects: []*models.Object{{Class: "Car", Properties: map[string]interface{}{"test": "test"}}, {Class: "Car", Properties: map[string]interface{}{"test": "error something"}}}, skip: []bool{false, false}, wantErrors: map[int]error{1: fmt.Errorf("something")}},
		{name: "first object errors", objects: []*models.Object{{Class: "Car", Properties: map[string]interface{}{"test": "error something"}}, {Class: "Car", Properties: map[string]interface{}{"test": "test"}}}, skip: []bool{false, false}, wantErrors: map[int]error{0: fmt.Errorf("something")}},
		{name: "vectorize all", objects: []*models.Object{{Class: "Car", Properties: map[string]interface{}{"test": "test"}}, {Class: "Car", Properties: map[string]interface{}{"test": "something"}}}, skip: []bool{false, false}},
		{name: "multiple vectorizer batches", objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "tokens 25"}}, // set limit so next 3 objects are one batch
			{Class: "Car", Properties: map[string]interface{}{"test": "first object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "second object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "third object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "first object second batch"}}, // rate is 100 again
			{Class: "Car", Properties: map[string]interface{}{"test": "second object second batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "third object second batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "fourth object second batch"}},
		}, skip: []bool{false, false, false, false, false, false, false, false}},
		{name: "multiple vectorizer batches with skips and errors", objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "tokens 25"}}, // set limit so next 3 objects are one batch
			{Class: "Car", Properties: map[string]interface{}{"test": "first object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "second object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "error something"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "first object second batch"}}, // rate is 100 again
			{Class: "Car", Properties: map[string]interface{}{"test": "second object second batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "third object second batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "fourth object second batch"}},
		}, skip: []bool{false, true, false, false, false, true, false, false}, wantErrors: map[int]error{3: fmt.Errorf("something")}},
		{name: "token too long", objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "tokens 5"}}, // set limit
			{Class: "Car", Properties: map[string]interface{}{"test": "long long long long, long, long, long, long"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "short"}},
		}, skip: []bool{false, false, false}, wantErrors: map[int]error{1: fmt.Errorf("text too long for vectorization")}},
		{name: "token too long, last item in batch", objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "tokens 5"}}, // set limit
			{Class: "Car", Properties: map[string]interface{}{"test": "short"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "long long long long, long, long, long, long"}},
		}, skip: []bool{false, false, false}, wantErrors: map[int]error{2: fmt.Errorf("text too long for vectorization")}},
		{name: "skip last item", objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "fir test object"}}, // set limit
			{Class: "Car", Properties: map[string]interface{}{"test": "first object first batch"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "second object first batch"}},
		}, skip: []bool{false, false, true}},
		{name: "deadline", deadline: 200 * time.Millisecond, objects: []*models.Object{
			{Class: "Car", Properties: map[string]interface{}{"test": "tokens 15"}}, // set limit so next two items are in a batch
			{Class: "Car", Properties: map[string]interface{}{"test": "wait 200"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "long long long long"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "next batch, will be aborted due to context deadline"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "skipped"}},
			{Class: "Car", Properties: map[string]interface{}{"test": "has error again"}},
		}, skip: []bool{false, false, false, false, true, false}, wantErrors: map[int]error{3: fmt.Errorf("context deadline exceeded or cancelled"), 5: fmt.Errorf("context deadline exceeded or cancelled")}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			v := New(client, 1*time.Second, logger) // avoid waiting for rate limit
			deadline := time.Now().Add(10 * time.Second)
			if tt.deadline != 0 {
				deadline = time.Now().Add(tt.deadline)
			}

			ctx, cancl := context.WithDeadline(context.Background(), deadline)
			vecs, errs := v.ObjectBatch(
				ctx, tt.objects, tt.skip, cfg,
			)

			require.Len(t, errs, len(tt.wantErrors))
			require.Len(t, vecs, len(tt.objects))

			for i := range tt.objects {
				if tt.wantErrors[i] != nil {
					require.Equal(t, tt.wantErrors[i], errs[i])
				} else if tt.skip[i] {
					require.Nil(t, vecs[i])
				} else {
					require.NotNil(t, vecs[i])
				}
			}
			cancl()
		})
	}
}

func TestBatchMultiple(t *testing.T) {
	client := &fakeBatchClient{}
	cfg := &fakeClassConfig{vectorizePropertyName: false, classConfig: map[string]interface{}{"vectorizeClassName": false}}
	logger, _ := test.NewNullLogger()

	v := New(client, 40*time.Second, logger)
	res := make(chan int, 3)
	wg := sync.WaitGroup{}
	wg.Add(3)

	// send multiple batches to the vectorizer and check if they are processed in the correct order. Note that the
	// ObjectBatch function is doing some work before the objects are send to vectorization, so we need to leave some
	// time to account for that
	for i := 0; i < 3; i++ {
		i := i
		go func() {
			vecs, errs := v.ObjectBatch(context.Background(), []*models.Object{
				{Class: "Car", Properties: map[string]interface{}{"test": "wait 100"}},
			}, []bool{false}, cfg)
			require.Len(t, vecs, 1)
			require.Len(t, errs, 0)
			res <- i
			wg.Done()
		}()

		time.Sleep(100 * time.Millisecond) // the vectorizer waits for 100ms with processing the object, so it is sa
	}

	wg.Wait()
	close(res)
	// check that the batches were processed in the correct order
	for i := 0; i < 3; i++ {
		require.Equal(t, i, <-res)
	}
}

func TestBatchTimeouts(t *testing.T) {
	client := &fakeBatchClient{defaultResetRate: 1}
	cfg := &fakeClassConfig{vectorizePropertyName: false, classConfig: map[string]interface{}{"vectorizeClassName": false}}
	logger, _ := test.NewNullLogger()

	cases := []struct {
		batchTime      time.Duration
		expectedErrors int
	}{
		{batchTime: 100 * time.Millisecond, expectedErrors: 1},
		{batchTime: 2 * time.Second, expectedErrors: 0},
	}
	for _, tt := range cases {
		t.Run("", func(t *testing.T) {
			v := New(client, tt.batchTime, logger)

			_, errs := v.ObjectBatch(context.Background(), []*models.Object{
				{Class: "Car", Properties: map[string]interface{}{"test": "tokens 13"}}, // first request, set rate down so the next two items can be sent
				{Class: "Car", Properties: map[string]interface{}{"test": "wait 90"}},   // second batch, use up batch time to trigger waiting for refresh
				{Class: "Car", Properties: map[string]interface{}{"test": "tokens 11"}}, // set next rate so the next object is too long. Depending on the total batch time it either sleeps or not
				{Class: "Car", Properties: map[string]interface{}{"test": "next batch long long long long long long"}},
			}, []bool{false, false, false, false}, cfg)

			require.Len(t, errs, tt.expectedErrors)
		})
	}
}

func TestBatchRequestLimit(t *testing.T) {
	client := &fakeBatchClient{defaultResetRate: 1}
	cfg := &fakeClassConfig{vectorizePropertyName: false, classConfig: map[string]interface{}{"vectorizeClassName": false}}
	thirtyTokens := "ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab ab"
	logger, _ := test.NewNullLogger()

	cases := []struct {
		batchTime      time.Duration
		expectedErrors int
	}{
		{batchTime: 100 * time.Millisecond, expectedErrors: 1},
		{batchTime: 2 * time.Second, expectedErrors: 0},
	}
	for _, tt := range cases {
		t.Run("", func(t *testing.T) {
			v := New(client, tt.batchTime, logger)

			_, errs := v.ObjectBatch(context.Background(), []*models.Object{
				{Class: "Car", Properties: map[string]interface{}{"test": "requests 0"}},                               // wait for the rate limit to reset
				{Class: "Car", Properties: map[string]interface{}{"test": "requests 0" + thirtyTokens + thirtyTokens}}, // fill up default limit of 100 tokens
			}, []bool{false, false, false, false}, cfg)
			require.Len(t, errs, tt.expectedErrors)
		})
	}
}
