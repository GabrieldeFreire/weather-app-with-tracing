package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// WeatherResponse representa a resposta com as temperaturas em diferentes unidades
type WeatherResponse struct {
	Localidade string  `json:"city"`
	TempC      float64 `json:"temp_C"`
	TempF      float64 `json:"temp_F"`
	TempK      float64 `json:"temp_K"`
}

var httpClient http.Client

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	traceProvider, err := initTracer(ctx, "service-b", "opentelemetry-collector:4317")
	if err != nil {
		panic(err)
	}

	defer func() {
		if err := traceProvider.Shutdown(ctx); err != nil {
			panic(err)
		}
	}()

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient = http.Client{Transport: tr}

	srv := &http.Server{
		Addr:         ":8000", // Server address
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 15 * time.Second,
		Handler:      http.HandlerFunc(getWeatherHandler),
	}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server ListenAndServe: %v", err)
		}
	}()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("HTTP server Shutdown: %v", err)
	}
}

func getWeatherHandler(w http.ResponseWriter, r *http.Request) {
	tracer := otel.Tracer("getWeatherHandler")

	carrier := propagation.HeaderCarrier(r.Header)
	ctx := r.Context()
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	ctx, span := tracer.Start(ctx, "getWeatherHandler")
	defer span.End()

	cep := r.URL.Query().Get("cep")
	if len(cep) != 8 {
		http.Error(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	location, err := getLocation(ctx, tracer, cep)
	if err != nil {
		http.Error(w, "can not find zipcode", http.StatusNotFound)
		return
	}

	tempC, err := getTemperature(ctx, tracer, location)
	if err != nil {
		http.Error(w, "error fetching temperature", http.StatusInternalServerError)
		return
	}

	tempF := tempC*1.8 + 32
	tempK := tempC + 273.15

	response := WeatherResponse{
		Localidade: location,
		TempC:      toFixed(tempC, 2),
		TempF:      toFixed(tempF, 2),
		TempK:      toFixed(tempK, 2),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getLocation(ctx context.Context, tracer trace.Tracer, cep string) (string, error) {
	ctx, span := tracer.Start(ctx, "getLocation from viacep")
	defer span.End()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://viacep.com.br/ws/%s/json/", cep), nil)
	if err != nil {
		return "", err
	}

	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", errors.New("invalid response from ViaCEP")
	}

	var result map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return "", err
	}

	localidade, ok := result["localidade"].(string)
	if !ok {
		return "", errors.New("localidade not found in response")
	}

	return localidade, nil
}

func getTemperature(ctx context.Context, tracer trace.Tracer, location string) (float64, error) {
	ctx, span := tracer.Start(ctx, "getTemperature from weatherapi")
	defer span.End()

	apiKey := os.Getenv("WEATHER_API_KEY")
	url := fmt.Sprintf("http://api.weatherapi.com/v1/current.json?key=%s&q=%s", apiKey, url.QueryEscape(location))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("invalid response from WeatherAPI")
	}

	var result map[string]interface{}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return 0, err
	}

	current, ok := result["current"].(map[string]interface{})
	if !ok {
		return 0, errors.New("current weather data not found in response")
	}

	tempC, ok := current["temp_c"].(float64)
	if !ok {
		tempCInt, ok := current["temp_c"].(int)
		if !ok {
			return 0, errors.New("temperature data not found in response")
		}
		tempC = float64(tempCInt)
	}

	return tempC, nil
}

func toFixed(num float64, precision int) float64 {
	precicionBase10 := math.Pow(10, float64(precision))
	return float64(math.Round(num*precicionBase10)) / precicionBase10
}

func initConn(serviceURL string) (*grpc.ClientConn, error) {
	conn, err := grpc.NewClient(
		serviceURL,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}
	return conn, nil
}

func initTracer(ctx context.Context, serviceName, serviceURL string) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(
		ctx,
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create tracer: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	conn, err := initConn(serviceURL)
	if err != nil {
		return nil, err
	}

	tracerExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(tracerExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(traceProvider)

	otel.SetTextMapPropagator(propagation.TraceContext{})

	return traceProvider, nil
}
