package driver

import (
	"context"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

func logInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).Infof("gRPC call: %s", info.FullMethod)
	klog.V(5).Infof("gRPC request: %+v", req)

	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("gRPC error: %s: %v", info.FullMethod, err)
	} else {
		klog.V(5).Infof("gRPC response: %+v", resp)
	}
	return resp, err
}
