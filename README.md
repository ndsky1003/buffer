#### 问题
1. 同一个bytes.Buffer使用的底层结构是同一个,包括其Bytes方法返回的[]byte
    1. 第一步获取一个buf,使用完了,返回这个buf.Bytes(),这个方法不是其copy,是共用底层空间的
    2. 第二步再获取一个buf,若果拿到了同一个第一步的buf,使用,就会对其第一步的返回结果进行修改


##### 综上问题,才有了这个项目,每次返回Bytes,都是一个copy,这样虽然性能有欠缺,但是心智不会过渡消耗

> 第一版是为了解决一些问题
```golang
type buffer struct {
	bytes.Buffer
}

var pool = &sync.Pool{
	New: func() any {
		return &buffer{}
	},
}

func Get() *buffer {
	return pool.Get().(*buffer)
}

func Release(b *buffer) {
	b.Release()
}

func (this *buffer) Release() {
	if this == nil {
		return
	}
	this.Reset()
	pool.Put(this)
}

// 默认的copy一份,避免共用底层,修改会影响当前数据
// this.Buffer.Bytes() 返回没copy的data
func (this *buffer) Bytes() []byte {
	bs := this.Buffer.Bytes()
	b := make([]byte, len(bs))
	copy(b, bs)
	return b
}
```
