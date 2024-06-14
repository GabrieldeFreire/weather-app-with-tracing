package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"
)

const (
	CEP_LENGTH      = 8
	getTempEndpoint = "http://service-b:8000/"
)

type Cep struct {
	Cep string `json:"cep"`
}

type CepResponse struct {
	Cep        string `json:"cep"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code"`
}

type TemperatureResponse struct {
	Localidade string  `json:"city"`
	TempC      float64 `json:"temp_C"`
	TempF      float64 `json:"temp_F"`
	TempK      float64 `json:"temp_K"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	traceProvider, err := initTracer(ctx, "service-a", "opentelemetry-collector:4317")
	if err != nil {
		panic(err)
	}

	defer func() {
		if err := traceProvider.Shutdown(ctx); err != nil {
			panic(err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /", postCepHandler)
	fmt.Println("Starting server at :8080")
	http.ListenAndServe(":8080", mux)
	srv := &http.Server{
		Addr:         ":8080", // Server address
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  5 * time.Second,  // Server read timeout
		WriteTimeout: 15 * time.Second, // Server write timeout
		Handler:      mux,              // HTTP handler
	}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server ListenAndServe: %v", err) // Log fatal errors during server startup
		}
	}()
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel() // Ensures cancel function is called on exit
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("HTTP server Shutdown: %v", err) // Log fatal errors during server shutdown
	}
}

func postCepHandler(w http.ResponseWriter, r *http.Request) {
	carrier := propagation.HeaderCarrier(r.Header)
	ctx := r.Context()
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)

	tracer := otel.Tracer("intput-api-tracer")
	ctx, span := tracer.Start(ctx, "request-temp-info-to-service-b")
	defer span.End()

	var c Cep
	err := json.NewDecoder(r.Body).Decode(&c)
	if err != nil || len(c.Cep) != CEP_LENGTH {
		http.Error(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	weatherUrl := fmt.Sprintf("%s?cep=%s", getTempEndpoint, c.Cep)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, weatherUrl, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("NewRequestWithContext service b error: %s", err.Error()), http.StatusInternalServerError)
		return
	}

	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("DefaultClient.Do service b error: %s", err.Error()), http.StatusInternalServerError)
		return
	}

	defer resp.Body.Close()

	var response TemperatureResponse

	json.NewDecoder(resp.Body).Decode(&response)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == http.StatusNotFound {
		http.Error(w, "can not find zipcode", http.StatusNotFound)
		return
	}
	json.NewEncoder(w).Encode(response)
}

func getTemperature(location string) (float64, error) {
	apiKey := os.Getenv("WEATHER_API_KEY")
	url := fmt.Sprintf("http://api.weatherapi.com/v1/current.json?key=%s&q=%s", apiKey, url.QueryEscape(location))
	resp, err := http.Get(url)
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

func initTracer(ctx context.Context, serviceName, serviceURL string) (*trace.TracerProvider, error) {
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

	traceProvider := trace.NewTracerProvider(
		trace.WithBatcher(tracerExporter),
		trace.WithResource(res),
		trace.WithSampler(trace.AlwaysSample()),
	)

	otel.SetTracerProvider(traceProvider)

	otel.SetTextMapPropagator(propagation.TraceContext{})

	return traceProvider, nil
}
