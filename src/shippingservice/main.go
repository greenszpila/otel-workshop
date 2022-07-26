// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
	"go.opentelemetry.io/otel/trace"

	// FOK Workshop - Span Attributes
	"go.opentelemetry.io/otel/attribute"

	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"

	pb "github.com/GoogleCloudPlatform/microservices-demo/src/shippingservice/genproto"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	defaultPort = "50051"
	serviceName = "shippingservice"
)

// Quote represents a currency value.
type Quote struct {
	Dollars uint32
	Cents   uint32
}

var log *logrus.Logger
var tracer trace.Tracer

func init() {
	log = logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout
}

func main() {
	initTracing()
	port := defaultPort
	if value, ok := os.LookupEnv("PORT"); ok {
		port = value
	}
	port = fmt.Sprintf(":%s", port)

	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	var srv = grpc.NewServer(
		grpc.UnaryInterceptor(otelgrpc.UnaryServerInterceptor()),
		grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
	)

	svc := &server{}
	pb.RegisterShippingServiceServer(srv, svc)
	healthpb.RegisterHealthServer(srv, svc)
	log.Infof("Shipping Service listening on port %s", port)

	// Register reflection service on gRPC server.
	reflection.Register(srv)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}
}

func initTracing() {
	res, err := detectResource()
	if err != nil {
		log.WithError(err).Fatal("failed to detect environment resource")
	}

	exp, err := spanExporter()
	if err != nil {
		log.WithError(err).Fatal("failed to initialize Span exporter")
		return
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exp)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	tracer = tp.Tracer("ExampleService")
}

func detectResource() (*resource.Resource, error) {
	appResource, err := resource.New(
		context.Background(),
		resource.WithAttributes(semconv.ServiceNameKey.String(serviceName)),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}
	return resource.Merge(resource.Default(), appResource)
}

func spanExporter() (*otlptrace.Exporter, error) {
	var otlpEndpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if otlpEndpoint != "" {
		log.Infof("exporting to OTLP collector at %s", otlpEndpoint)
		traceClient := otlptracegrpc.NewClient(
			otlptracegrpc.WithInsecure(),
			otlptracegrpc.WithEndpoint(otlpEndpoint),
		)
		return otlptrace.New(context.Background(), traceClient)
	}
	return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT must not be empty")
}

// server controls RPC service responses.
type server struct{}

// Check is for health checking.
func (s *server) Check(ctx context.Context, req *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

func (s *server) Watch(req *healthpb.HealthCheckRequest, ws healthpb.Health_WatchServer) error {
	return status.Errorf(codes.Unimplemented, "health check via Watch not implemented")
}

// GetQuote produces a shipping quote (cost) in USD.
func (s *server) GetQuote(ctx context.Context, in *pb.GetQuoteRequest) (*pb.GetQuoteResponse, error) {
	
	log.Info("[GetQuote] received request")
	defer log.Info("[GetQuote] completed request")

	// FOK Workshop - Building Spans
	quote := CreateQuoteFromCount(0, ctx)

	// Generate a response.
	return &pb.GetQuoteResponse{
		CostUsd: &pb.Money{
			CurrencyCode: "USD",
			Units:        int64(quote.Dollars),
			Nanos:        int32(quote.Cents * 10000000)},
	}, nil

}

// ShipOrder mocks that the requested items will be shipped.
// It supplies a tracking ID for notional lookup of shipment delivery status.
func (s *server) ShipOrder(ctx context.Context, in *pb.ShipOrderRequest) (*pb.ShipOrderResponse, error) {
	
	// FOK Workshop - Span Attributes
	ctx, parentSpan := tracer.Start(ctx, "shipOrder")
	defer parentSpan.End()

	log.Info("[ShipOrder] received request")
	defer log.Info("[ShipOrder] completed request")
	
	// 1. Create a Tracking ID
	baseAddress := fmt.Sprintf("%s, %s, %s, %d", in.Address.StreetAddress, in.Address.City, in.Address.State, in.Address.ZipCode)
	
	// FOK Workshop - Span Attributes
	parentSpan.SetAttributes(
		attribute.String("address", baseAddress), 
		attribute.String("city", in.Address.City), 
		attribute.String("state", in.Address.State))
	
	// FOK Workshop - Adding Errors
	if in.Address.ZipCode < 10000 || in.Address.ZipCode > 99999 {
		parentSpan.SetStatus(1, "zipcode is invalid")
	}

	id := CreateTrackingId(baseAddress)

	// 2. Generate a response.
	return &pb.ShipOrderResponse{
		TrackingId: id,
	}, nil
}

// String representation of the Quote.
func (q Quote) String() string {
	return fmt.Sprintf("$%d.%d", q.Dollars, q.Cents)
}

// CreateQuoteFromCount takes a number of items and returns a Price struct.
// FOK Workshop - Building spans
func CreateQuoteFromCount(count int, ctx context.Context) Quote {

	// FOK Workshop - Building Spans
	ctx, childSpan := tracer.Start(ctx, "CreateQuoteFromCount")
	defer childSpan.End()

	// FOK Workshop - Adding a Delay
	time.Sleep(time.Second / 10)

	// FOK Workshop - Building Spans
	return CreateQuoteFromFloat(float64(rand.Intn(100)), ctx)
}

// CreateQuoteFromFloat takes a price represented as a float and creates a Price struct.
// FOK Workshop - Building Spans
func CreateQuoteFromFloat(value float64, ctx context.Context) Quote {
	
	// FOK Workshop - Building Spans
	ctx, childSpan := tracer.Start(ctx, "CreateQuoteFromFloat")
	defer childSpan.End()

	// FOK Workshop - Adding a Delay
	time.Sleep(time.Second / 3)

	units, fraction := math.Modf(value)
	return Quote{
		uint32(units),
		uint32(math.Trunc(fraction * 100)),
	}
}
