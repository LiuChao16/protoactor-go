package consul

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/AsynkronIT/protoactor-go/cluster"
	"github.com/hashicorp/consul/api"
)

var (
	ProviderShuttingDownError = fmt.Errorf("consul cluster provider is shutting down")
	// for mocking purposes this function is assigned to a variable
	blockingUpdateTTLFunc = blockingUpdateTTL
)

type Provider struct {
	cluster               *cluster.Cluster
	deregistered          bool
	shutdown              bool
	id                    string
	clusterName           string
	address               string
	port                  int
	knownKinds            []string
	index                 uint64 // consul blocking index
	client                *api.Client
	ttl                   time.Duration
	refreshTTL            time.Duration
	updateTTLWaitGroup    sync.WaitGroup
	deregisterCritical    time.Duration
	blockingWaitTime      time.Duration
	statusValue           cluster.MemberStatusValue
	statusValueSerializer cluster.MemberStatusValueSerializer
	clusterError          error
}

func New() (*Provider, error) {
	return NewWithConfig(&api.Config{})
}

func NewWithConfig(consulConfig *api.Config) (*Provider, error) {
	client, err := api.NewClient(consulConfig)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		client:             client,
		ttl:                3 * time.Second,
		refreshTTL:         1 * time.Second,
		deregisterCritical: 60 * time.Second,
		blockingWaitTime:   20 * time.Second,
	}
	return p, nil
}

func (p *Provider) RegisterMember(cluster *cluster.Cluster, clusterName string, address string, port int, knownKinds []string,
	statusValue cluster.MemberStatusValue, serializer cluster.MemberStatusValueSerializer) error {
	p.cluster = cluster
	p.id = fmt.Sprintf("%v@%v:%v", clusterName, address, port)
	p.clusterName = clusterName
	p.address = address
	p.port = port
	p.knownKinds = knownKinds
	p.statusValue = statusValue
	p.statusValueSerializer = serializer

	err := p.registerService()
	if err != nil {
		return err
	}

	// IMPORTANT: do these ops sync directly after registering.
	// this will ensure that the local node sees its own information upon startup.

	// force our own TTL to be OK
	err = blockingUpdateTTLFunc(p)
	if err != nil {
		return err
	}

	// force our own existence to be part of the first status update
	p.blockingStatusChange()

	p.UpdateTTL()
	return nil
}

func (p *Provider) DeregisterMember() error {
	err := p.deregisterService()
	if err != nil {
		fmt.Println(err)
		return err
	}
	p.deregistered = true
	return nil
}

func (p *Provider) Shutdown() error {
	if p.shutdown {
		return nil
	}

	p.shutdown = true
	p.updateTTLWaitGroup.Wait()

	if !p.deregistered {
		err := p.DeregisterMember()
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Provider) UpdateTTL() {
	go func() {
		p.updateTTLWaitGroup.Add(1)
		defer p.updateTTLWaitGroup.Done()

	OUTER:
		for !p.shutdown {

			err := blockingUpdateTTLFunc(p)
			if err == nil {
				time.Sleep(p.refreshTTL)
				continue
			}

			log.Println("[CLUSTER] [CONSUL] Failure refreshing service TTL. Trying to reregister service if not in consul.")

			services, err := p.client.Agent().Services()
			for id := range services {
				if id == p.id {
					log.Println("[CLUSTER] [CONSUL] Service found in consul -> doing nothing")
					time.Sleep(p.refreshTTL)
					continue OUTER
				}
			}

			err = p.registerService()
			if err != nil {
				log.Println("[CLUSTER] [CONSUL] Error reregistering service ", err)
				time.Sleep(p.refreshTTL)
				continue
			}

			log.Println("[CLUSTER] [CONSUL] Reregistered service in consul")
			time.Sleep(p.refreshTTL)
		}
	}()
}

func (p *Provider) UpdateMemberStatusValue(statusValue cluster.MemberStatusValue) error {
	p.statusValue = statusValue
	if p.statusValue == nil {
		return nil
	}
	if p.shutdown {
		// don't re-register when already in the process of shutting down
		return ProviderShuttingDownError
	}

	// Register service again to update the status value
	return p.registerService()
}

func blockingUpdateTTL(p *Provider) error {
	p.clusterError = p.client.Agent().UpdateTTL("service:"+p.id, "", api.HealthPassing)
	return p.clusterError
}

func (p *Provider) registerService() error {
	s := &api.AgentServiceRegistration{
		ID:      p.id,
		Name:    p.clusterName,
		Tags:    p.knownKinds,
		Address: p.address,
		Port:    p.port,
		Meta: map[string]string{
			"StatusValue": p.statusValueSerializer.Serialize(p.statusValue),
		},
		Check: &api.AgentServiceCheck{
			DeregisterCriticalServiceAfter: p.deregisterCritical.String(),
			TTL:                            p.ttl.String(),
		},
	}
	return p.client.Agent().ServiceRegister(s)
}

func (p *Provider) deregisterService() error {
	return p.client.Agent().ServiceDeregister(p.id)
}

// call this directly after registering the service
func (p *Provider) blockingStatusChange() {
	p.notifyStatuses()
}

func (p *Provider) notifyStatuses() {
	statuses, meta, err := p.client.Health().Service(p.clusterName, "", false, &api.QueryOptions{
		WaitIndex: p.index,
		WaitTime:  p.blockingWaitTime,
	})
	if err != nil {
		log.Printf("Error %v", err)
		return
	}
	p.index = meta.LastIndex

	res := make(cluster.TopologyEvent, len(statuses))
	for i, v := range statuses {
		key := fmt.Sprintf("%v/%v:%v", p.clusterName, v.Service.Address, v.Service.Port)
		memberID := key
		memberStatusVal := p.statusValueSerializer.Deserialize(v.Service.Meta["StatusValue"])
		ms := &cluster.MemberStatus{
			MemberID:    memberID,
			Host:        v.Service.Address,
			Port:        v.Service.Port,
			Kinds:       v.Service.Tags,
			Alive:       len(v.Checks) > 0 && v.Checks.AggregatedStatus() == api.HealthPassing,
			StatusValue: memberStatusVal,
		}
		res[i] = ms

		// Update Tags for this member
		if memberID == p.id {
			p.knownKinds = v.Service.Tags
		}
	}
	// the reason why we want this in a batch and not as individual messages is that
	// if we have an atomic batch, we can calculate what nodes have left the cluster
	// passing events one by one, we can't know if someone left or just haven't changed status for a long time

	// publish the current cluster topology onto the event stream
	p.cluster.ActorSystem.EventStream.Publish(res)
}

func (p *Provider) MonitorMemberStatusChanges() {
	go func() {
		for !p.shutdown {
			p.notifyStatuses()
		}
	}()
}

// GetHealthStatus returns an error if the cluster health status has problems
func (p *Provider) GetHealthStatus() error {
	return p.clusterError
}
