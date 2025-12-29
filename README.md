#### NOTE:
> 这个库使用了`sync.Pool` 依赖于应用层的gc，会导致恶性循环


#### 安装
```golang
go get  github.com/ndsky1003/buffer/v2
```
#### usage
```golang
var globalPool = buffer.NewBufferPool(
	buffer.Options().
		SetMinSize(512).          // 最小 512B
		SetMaxSize(64 << 20).     // 最大 64MB
		SetMaxPercent(1.5).       // 推荐：1.5 倍冗余 (比之前的 2.0 更激进一点，利于回收)
		SetCalibratePeriod(1000), // 每 1000 次调用校准一次
)
```
