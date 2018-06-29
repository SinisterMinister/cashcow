package main

import (
	"os"
	"sync"

	"github.com/sinisterminister/coinfactory"

	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
)

type orderMorgue struct {
	BuyOrders  []coinfactory.OrderRequest
	SellOrders []coinfactory.OrderRequest
}

type orderCoffin struct {
	Key string `json:"key"`
	coinfactory.OrderRequest
}

func createOrderCoffin(order coinfactory.OrderRequest) orderCoffin {
	return orderCoffin{order.Symbol + order.Side, order}
}

type orderNecromancer struct {
	deadOrders map[string]orderMorgue
	db         *dynamodb.DynamoDB
}

var orderNecromancerOnce = sync.Once{}
var orderNecromancerInstance *orderNecromancer

func getOrderNecromancerInstance() *orderNecromancer {
	orderNecromancerOnce.Do(func() {
		// Tell AWS to use the credentials file for region
		os.Setenv("AWS_SDK_LOAD_CONFIG", "true")

		// Get an AWS session
		sess, err := session.NewSession(&aws.Config{})
		if err != nil {
			log.Fatal("Could not get AWS session")
		}

		// Create DynamoDB client
		svc := dynamodb.New(sess)

		orderNecromancerInstance = &orderNecromancer{
			db:         svc,
			deadOrders: make(map[string]orderMorgue),
		}

	})

	return orderNecromancerInstance
}

func (n *orderNecromancer) fetchDeadOrders() {}

func (n *orderNecromancer) collectOrder(order *coinfactory.Order) {}

// func (n *orderNecromancer) recordOrder(coffin orderCoffin) error {}

// func (n *orderNecromancer) purgeOrder(coffin orderCoffin) {}

// func (n *orderNecromancer) updateOrder(coffin orderCoffin) error {}
