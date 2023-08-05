package eureka

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hudl/fargo"
	"istio.io/api/networking/v1alpha3"
	versionedclient "istio.io/client-go/pkg/clientset/versioned"
	"istio.io/pkg/log"
	ctrl "sigs.k8s.io/controller-runtime"

	apiv1 "github.com/alibaba/higress/api/networking/v1"
	"github.com/alibaba/higress/pkg/common"
	provider "github.com/alibaba/higress/registry"
	. "github.com/alibaba/higress/registry/eureka/client"
	"github.com/alibaba/higress/registry/memory"
)

const (
	DefaultFullRefreshIntervalLimit = time.Second * 30
	suffix                          = "eureka"
)

type watcher struct {
	provider.BaseWatcher
	apiv1.RegistryConfig

	WatchingServices     map[string]*fargo.Application `json:"watching_services"`
	RegistryType         provider.ServiceRegistryType  `json:"registry_type"`
	Status               provider.WatcherStatus        `json:"status"`
	cache                memory.Cache
	mutex                *sync.Mutex
	stop                 chan struct{}
	istioClient          *versionedclient.Clientset
	isStop               bool
	updateCacheWhenEmpty bool

	watchers                  map[string]*Plan
	eurekaClient              EurekaHttpClient
	fullRefreshIntervalLimit  time.Duration
	deltaRefreshIntervalLimit time.Duration
}

type WatcherOption func(w *watcher)

func NewWatcher(cache memory.Cache, opts ...WatcherOption) (provider.Watcher, error) {
	w := &watcher{
		WatchingServices: make(map[string]*fargo.Application),
		RegistryType:     provider.Eureka,
		Status:           provider.UnHealthy,
		cache:            cache,
		mutex:            &sync.Mutex{},
		stop:             make(chan struct{}),
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}

	ic, err := versionedclient.NewForConfig(config)
	if err != nil {
		log.Errorf("can not new istio client, err:%v", err)
		return nil, err
	}
	w.istioClient = ic

	w.fullRefreshIntervalLimit = DefaultFullRefreshIntervalLimit

	for _, opt := range opts {
		opt(w)
	}

	cfg := NewDefaultConfig()
	cfg.BaseUrl = net.JoinHostPort(w.Domain, strconv.FormatUint(uint64(w.Port), 10))
	w.eurekaClient = NewEurekaHttpClient(cfg)

	return w, nil
}

func WithEurekaFullRefreshInterval(refreshInterval int64) WatcherOption {
	return func(w *watcher) {
		if refreshInterval < int64(DefaultFullRefreshIntervalLimit) {
			refreshInterval = int64(DefaultFullRefreshIntervalLimit)
		}
		w.fullRefreshIntervalLimit = time.Duration(refreshInterval)
	}
}

func WithType(t string) WatcherOption {
	return func(w *watcher) {
		w.Type = t
	}
}

func WithName(name string) WatcherOption {
	return func(w *watcher) {
		w.Name = name
	}
}

func WithDomain(domain string) WatcherOption {
	return func(w *watcher) {
		w.Domain = domain
	}
}

func WithPort(port uint32) WatcherOption {
	return func(w *watcher) {
		w.Port = port
	}
}

func WithUpdateCacheWhenEmpty(enable bool) WatcherOption {
	return func(w *watcher) {
		w.updateCacheWhenEmpty = enable
	}
}

func (w *watcher) Run() {
	ticker := time.NewTicker(DefaultFullRefreshIntervalLimit)
	defer ticker.Stop()

	w.Status = provider.ProbeWatcherStatus(w.Domain, strconv.FormatUint(uint64(w.Port), 10))
	w.doFullRefresh()
	w.Ready(true)

	for {
		select {
		case <-ticker.C:
			w.doFullRefresh()
		case <-w.stop:
			log.Info("eureka watcher(%v) is stopping ...", w.Name)
			return
		}
	}
}

func (w *watcher) Stop() {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	for serviceName := range w.WatchingServices {
		if err := w.unsubscribe(serviceName); err != nil {
			delete(w.WatchingServices, serviceName)
		}
		w.cache.DeleteServiceEntryWrapper(makeHost(serviceName))
	}
}

func (w *watcher) IsHealthy() bool {
	return w.Status == provider.Healthy
}

func (w *watcher) GetRegistryType() string {
	return w.RegistryType.String()
}

// doFullRefresh todo(lql): it's better to support deltaRefresh
func (w *watcher) doFullRefresh() {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	applications, err := w.eurekaClient.GetApplications()
	if err != nil {
		log.Errorf("Failed to full fetch eureka services, error : %v", err)
		return
	}

	fetchedServices := applications.Apps
	for serviceName := range w.WatchingServices {
		if _, ok := fetchedServices[serviceName]; !ok {
			if err = w.unsubscribe(serviceName); err != nil {
				log.Errorf("Failed to unsubscribe service %v, error : %v", serviceName, err)
				continue
			}
			delete(w.WatchingServices, serviceName)
		}
	}

	for serviceName := range fetchedServices {
		if _, ok := w.WatchingServices[serviceName]; !ok {
			if err = w.subscribe(fetchedServices[serviceName]); err != nil {
				log.Errorf("Failed to subscribe service %v, error : %v", serviceName, err)
				continue
			}
			w.WatchingServices[serviceName] = fetchedServices[serviceName]
		}
	}
}

func (w *watcher) subscribe(service *fargo.Application) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if _, ok := w.WatchingServices[service.Name]; !ok {
		w.watchers[service.Name] = NewPlan(w.eurekaClient, service.Name, func(service *fargo.Application) error {
			defer w.UpdateService()

			if len(service.Instances) != 0 {
				se := generateServiceEntry(service)
				w.cache.UpdateServiceEntryWrapper(makeHost(service.Name), &memory.ServiceEntryWrapper{
					ServiceName:  service.Name,
					ServiceEntry: se,
					Suffix:       suffix,
					RegistryType: w.Type,
				})
			}

			if w.updateCacheWhenEmpty {
				w.cache.DeleteServiceEntryWrapper(makeHost(service.Name))
			}

			return nil
		})
	}
	return nil
}

func (w *watcher) unsubscribe(serviceName string) error {
	w.mutex.Lock()
	defer w.mutex.Unlock()

	if _, ok := w.watchers[serviceName]; !ok {
		return fmt.Errorf("%s not found in watchers", serviceName)
	}

	w.watchers[serviceName].Stop()
	delete(w.watchers, serviceName)
	w.UpdateService()

	return nil
}

func makeHost(serviceName string) string {
	return serviceName + common.DotSeparator + suffix
}

func convertMap(m map[string]interface{}) map[string]string {
	result := make(map[string]string)
	for k, v := range m {
		if value, ok := v.(string); ok {
			result[k] = value
		}
	}

	return result
}

func generateServiceEntry(app *fargo.Application) *v1alpha3.ServiceEntry {
	portList := make([]*v1alpha3.Port, 0)
	endpoints := make([]*v1alpha3.WorkloadEntry, 0)

	for _, instance := range app.Instances {
		protocol := common.HTTP
		if val, _ := instance.Metadata.GetString("protocol"); val != "" {
			protocol = common.ParseProtocol(val)
		}
		port := &v1alpha3.Port{
			Name:     protocol.String(),
			Number:   uint32(instance.Port),
			Protocol: protocol.String(),
		}
		if len(portList) == 0 {
			portList = append(portList, port)
		}
		endpoint := v1alpha3.WorkloadEntry{
			Address: instance.IPAddr,
			Ports:   map[string]uint32{port.Protocol: port.Number},
			Labels:  convertMap(instance.Metadata.GetMap()),
		}
		endpoints = append(endpoints, &endpoint)
	}

	se := &v1alpha3.ServiceEntry{
		Hosts:      []string{makeHost(app.Name)},
		Ports:      portList,
		Location:   v1alpha3.ServiceEntry_MESH_INTERNAL,
		Resolution: v1alpha3.ServiceEntry_STATIC,
		Endpoints:  endpoints,
	}

	return se
}
