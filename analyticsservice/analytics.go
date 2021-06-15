package analyticsservice

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/drone/ff-golang-server-sdk/evaluation"
	"github.com/drone/ff-golang-server-sdk/metricsclient"

	"github.com/drone/ff-golang-server-sdk/logger"
)

const (
	ffMetricType                 string = "FFMETRICS"
	featureIdentifierAttribute   string = "featureIdentifier"
	featureNameAttribute         string = "featureName"
	variationIdentifierAttribute string = "variationIdentifier"
	targetAttribute              string = "target"
	sdkVersionAttribute          string = "SDK_VERSION"
	sdkVersion                   string = "1.0.0"
	sdkTypeAttribute             string = "SDK_TYPE"
	sdkType                      string = "server"
	sdkLanguageAttribute         string = "SDK_LANGUAGE"
	sdkLanguage                  string = "go"
)

type analyticsEvent struct {
	target        *evaluation.Target
	featureConfig evaluation.FeatureConfig
	variation     evaluation.Variation
	count         int
}

// AnalyticsService provides a way to cache and send analytics to the server
type AnalyticsService struct {
	mx            *sync.Mutex
	analyticsChan chan analyticsEvent
	analyticsData map[string]analyticsEvent
	timeout       time.Duration
	logger        logger.Logger
	metricsClient *metricsclient.ClientWithResponsesInterface
	environmentID string
}

// NewAnalyticsService creates and starts a analytics service to send data to the client
func NewAnalyticsService(timeout time.Duration, logger logger.Logger) *AnalyticsService {
	serviceTimeout := timeout
	if timeout < 60*time.Second {
		serviceTimeout = 60 * time.Second
	} else if timeout > 1*time.Hour {
		serviceTimeout = 1 * time.Hour
	}
	as := AnalyticsService{
		mx:            &sync.Mutex{},
		analyticsChan: make(chan analyticsEvent),
		analyticsData: map[string]analyticsEvent{},
		timeout:       serviceTimeout,
		logger:        logger,
	}
	go as.listener()

	return &as
}

// Start starts the client and timer to send analytics
func (as *AnalyticsService) Start(ctx context.Context, client *metricsclient.ClientWithResponsesInterface, environmentID string) {
	as.metricsClient = client
	as.environmentID = environmentID
	go as.startTimer(ctx)
}

func (as *AnalyticsService) startTimer(ctx context.Context) {
timerloop:
	for {
		select {
		case <-time.After(as.timeout):
			as.sendDataAndResetCache(ctx)
		case <-ctx.Done():
			close(as.analyticsChan)
			break timerloop
		}
	}
}

// PushToQueue is used to queue analytics data to send to the server
func (as *AnalyticsService) PushToQueue(target *evaluation.Target, featureConfig *evaluation.FeatureConfig, variation evaluation.Variation) {
	fc := evaluation.FeatureConfig{}
	if featureConfig != nil {
		fc = *featureConfig
	}

	ad := analyticsEvent{
		target:        target,
		featureConfig: fc,
		variation:     variation,
	}
	as.analyticsChan <- ad
}

func (as *AnalyticsService) listener() {
	as.logger.Info("Analytics cache successfully initialized")
	for ad := range as.analyticsChan {
		targetIdentifier := ""
		if ad.target != nil {
			targetIdentifier = ad.target.Identifier
		}
		key := fmt.Sprintf("%s-%s-%s", ad.featureConfig.Feature, ad.variation.Identifier, targetIdentifier)

		as.mx.Lock()
		analytic, ok := as.analyticsData[key]
		if !ok {
			ad.count = 1
			as.analyticsData[key] = ad
		} else {
			ad.count = (analytic.count + 1)
			as.analyticsData[key] = ad
		}
		as.mx.Unlock()
	}
}

func (as *AnalyticsService) sendDataAndResetCache(ctx context.Context) {
	as.mx.Lock()
	// copy cache to send to server
	analyticsData := as.analyticsData
	// clear cache. As metrics is secondary to the flags, we do it this way
	// so it doesn't effect the performance of our users code. Even if it means
	// we lose metrics the odd time.
	as.analyticsData = map[string]analyticsEvent{}
	as.mx.Unlock()

	metricData := make([]metricsclient.MetricsData, 0, len(as.analyticsData))
	targetData := make([]metricsclient.TargetData, 0, len(as.analyticsData))

	for _, analytic := range analyticsData {
		if analytic.target != nil {
			if analytic.target.Anonymous == nil || !*analytic.target.Anonymous {
				var targetAttributes []metricsclient.KeyValue
				if analytic.target.Attributes != nil {
					targetAttributes = make([]metricsclient.KeyValue, 0, len(*analytic.target.Attributes))
					for key, value := range *analytic.target.Attributes {
						v, _ := value.(string)
						kv := metricsclient.KeyValue{
							Key:   key,
							Value: v,
						}
						targetAttributes = append(targetAttributes, kv)
					}

				}

				targetName := analytic.target.Identifier
				if analytic.target.Name != nil {
					targetName = *analytic.target.Name
				}

				td := metricsclient.TargetData{
					Name:       targetName,
					Identifier: analytic.target.Identifier,
					Attributes: targetAttributes,
				}
				targetData = append(targetData, td)
			}
		}

		metricAttributes := []metricsclient.KeyValue{
			{
				Key:   featureIdentifierAttribute,
				Value: analytic.featureConfig.Feature,
			},
			{
				Key:   featureNameAttribute,
				Value: analytic.featureConfig.Feature,
			},
			{
				Key:   variationIdentifierAttribute,
				Value: analytic.variation.Identifier,
			},
			{
				Key:   sdkTypeAttribute,
				Value: sdkType,
			},
			{
				Key:   sdkLanguageAttribute,
				Value: sdkLanguage,
			},
			{
				Key:   sdkVersionAttribute,
				Value: sdkVersion,
			},
		}

		if analytic.target != nil {
			metricAttributes = append(metricAttributes, metricsclient.KeyValue{
				Key:   targetAttribute,
				Value: analytic.target.Identifier,
			})
		}

		md := metricsclient.MetricsData{
			Timestamp:   time.Now().UnixNano() / (int64(time.Millisecond) / int64(time.Nanosecond)),
			Count:       analytic.count,
			MetricsType: ffMetricType,
			Attributes:  metricAttributes,
		}
		metricData = append(metricData, md)
	}

	// if targets data is empty we just send nil
	var targetDataPayload *[]metricsclient.TargetData = nil
	if len(targetData) > 0 {
		targetDataPayload = &targetData
	}

	analyticsPayload := metricsclient.PostMetricsJSONRequestBody{
		MetricsData: &metricData,
		TargetData:  targetDataPayload,
	}

	//jsonData, err := json.Marshal(analyticsPayload)
	//if err != nil {
	//	as.logger.Errorf(err.Error())
	//}
	//fmt.Println(string(jsonData))

	if as.metricsClient != nil {
		mClient := *as.metricsClient
		resp, err := mClient.PostMetricsWithResponse(ctx, metricsclient.EnvironmentPathParam(as.environmentID), analyticsPayload)
		if err != nil {
			as.logger.Error(err)
			return
		}
		if resp == nil {
			as.logger.Error("Empty response from metrics server")
			return
		}
		if resp.StatusCode() != 200 {
			as.logger.Errorf("Non 200 response from metrics server: %d", resp.StatusCode())
			return
		}

		as.logger.Info("Metrics sent to server")
	} else {
		as.logger.Warn("metrics client is not set")
	}
}
