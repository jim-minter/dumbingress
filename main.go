package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var cli *kubernetes.Clientset
var namespace string
var externalIP string

var m sync.Mutex
var ingresses = map[string]string{}
var services = map[string]string{}

func updateIngress(i *v1beta1.Ingress) (err error) {
	m.Lock()
	for _, r := range i.Spec.Rules {
		ingresses[r.Host] = fmt.Sprintf("%s/%s/%s", i.Namespace, r.HTTP.Paths[0].Backend.ServiceName, r.HTTP.Paths[0].Backend.ServicePort.String())
	}
	m.Unlock()

	if len(i.Status.LoadBalancer.Ingress) != 1 || i.Status.LoadBalancer.Ingress[0].IP != externalIP {
		i.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: externalIP}}
		_, err = cli.ExtensionsV1beta1().Ingresses(i.Namespace).UpdateStatus(i)
		if kerrors.IsConflict(err) {
			err = nil
		}
	}

	return
}

func watchIngresses() error {
	for {
		l, err := cli.ExtensionsV1beta1().Ingresses("").List(metav1.ListOptions{})
		if err != nil {
			return err
		}

		m.Lock()
		ingresses = map[string]string{}
		m.Unlock()
		for _, i := range l.Items {
			err := updateIngress(&i)
			if err != nil {
				return err
			}
		}

		w, err := cli.ExtensionsV1beta1().Ingresses("").Watch(metav1.ListOptions{ResourceVersion: l.ResourceVersion})
		if err != nil {
			return err
		}

	out:
		for {
			select {
			case event, ok := <-w.ResultChan():
				if !ok {
					break out
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					err := updateIngress(event.Object.(*v1beta1.Ingress))
					if err != nil {
						return err
					}

				case watch.Deleted:
					m.Lock()
					for _, r := range event.Object.(*v1beta1.Ingress).Spec.Rules {
						delete(ingresses, r.Host)
					}
					m.Unlock()

				case watch.Error:
					log.Println(event.Object)
					break out
				}
			}
		}
	}
}

func watchServices() error {
	for {
		l, err := cli.CoreV1().Services("").List(metav1.ListOptions{})
		if err != nil {
			return err
		}

		m.Lock()
		services = map[string]string{}
		for _, s := range l.Items {
			for _, p := range s.Spec.Ports {
				services[fmt.Sprintf("%s/%s/%d", s.Namespace, s.Name, p.Port)] = fmt.Sprintf("%s:%d", s.Spec.ClusterIP, p.Port)
			}
		}
		m.Unlock()

		w, err := cli.CoreV1().Services("").Watch(metav1.ListOptions{ResourceVersion: l.ResourceVersion})
		if err != nil {
			return err
		}

	out:
		for {
			select {
			case event, ok := <-w.ResultChan():
				if !ok {
					break out
				}

				switch event.Type {
				case watch.Added, watch.Modified:
					s := event.Object.(*v1.Service)
					m.Lock()
					for _, p := range s.Spec.Ports {
						services[fmt.Sprintf("%s/%s/%d", s.Namespace, s.Name, p.Port)] = fmt.Sprintf("%s:%d", s.Spec.ClusterIP, p.Port)
					}
					m.Unlock()

				case watch.Deleted:
					s := event.Object.(*v1.Service)
					m.Lock()
					for _, p := range s.Spec.Ports {
						delete(services, fmt.Sprintf("%s/%s/%d", s.Namespace, s.Name, p.Port))
					}
					m.Unlock()

				case watch.Error:
					log.Println(event.Object)
					break out
				}
			}
		}
	}
}

func getClients() error {
	clientconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	restconfig, err := clientconfig.ClientConfig()
	if err != nil {
		return err
	}

	namespace, _, err = clientconfig.Namespace()
	if err != nil {
		return err
	}

	cli, err = kubernetes.NewForConfig(restconfig)
	return err
}

func getExternalIP() {
	for {
		svc, err := cli.CoreV1().Services(namespace).Get("dumbingress", metav1.GetOptions{})
		if err == nil && len(svc.Status.LoadBalancer.Ingress) == 1 {
			externalIP = svc.Status.LoadBalancer.Ingress[0].IP
			log.Printf("got external IP %s\n", externalIP)
			return
		}

		time.Sleep(time.Second)
	}
}

type conn struct {
	net.Conn
	buf *bytes.Buffer
}

func (c *conn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if c.buf != nil {
		c.buf.Write(b[:n])
	}
	return n, err
}

func (c *conn) Write(b []byte) (int, error) {
	if c.buf != nil {
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func handle(c net.Conn) error {
	cc := &conn{Conn: c, buf: &bytes.Buffer{}}
	defer cc.Close()

	fakeError := errors.New("")
	var serverName string
	tc := tls.Server(cc, &tls.Config{
		GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
			serverName = chi.ServerName
			return nil, fakeError
		},
	})
	err := tc.Handshake()
	if err != fakeError {
		return err
	}

	m.Lock()
	dest := services[ingresses[serverName]]
	m.Unlock()

	c2, err := net.Dial("tcp4", dest)
	if err != nil {
		return err
	}
	defer c2.Close()

	_, err = c2.Write(cc.buf.Bytes())
	if err != nil {
		return err
	}
	cc.buf = nil

	errch := make(chan error, 1)
	go func() {
		_, err := io.Copy(c2, c)
		errch <- err
	}()

	_, err = io.Copy(c, c2)
	if err != nil {
		return err
	}

	return <-errch
}

func run() error {
	err := getClients()
	if err != nil {
		return err
	}

	getExternalIP()

	go func() {
		log.Println(watchIngresses())
	}()

	go func() {
		log.Println(watchServices())
	}()

	l, err := net.Listen("tcp4", "0.0.0.0:8443")
	if err != nil {
		return err
	}

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}

		go func() {
			err := handle(c)
			if err != nil && err != io.EOF {
				log.Println(err)
			}
		}()
	}
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}
