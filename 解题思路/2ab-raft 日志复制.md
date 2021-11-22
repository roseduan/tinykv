领导者选举出来之后，raft 集群就能开始对外提供服务了。客户端的请求会先到 Leader 节点，然后 Leader 会将数据组织成日志的方式，然后追加到本地状态机中。

下图是 raft 中日志的组织形式：

![img](https://cdn.nlark.com/yuque/0/2021/png/12925940/1637312318178-b885c257-2e1c-463d-aceb-a2e128a4aeff.png)

Leader 会发送日志项到其他 Follower 节点中，如果超过半数节点接收到消息，那么 Leader 会认为该条日志被成功提交了，然后它会更新自己日志项的 committed 信息。

Leader 只会追加自己的日志项，并且通过强制覆盖 Follower 节点的日志来保持各个节点日志的一致性。



再来看看代码实现。



一条日志在 raft 中被封装成了一个 Entry 的数据结构：

```plain
type Entry struct {
	EntryType            EntryType `protobuf:"varint,1"`
	Term                 uint64    `protobuf:"varint,2"`
	Index                uint64    `protobuf:"varint,3"`
	Data                 []byte    `protobuf:"bytes,4"`
}
```

可以看到 Entry 有四个主要的字段：

- EntryType：日志项的类型，目前只有 EntryNormal 和 EntryConfChange 这两种

- Term：日志的任期号，表示日志是在哪个任期生成的
- Index：日志的唯一编号，是一个递增的数字。有了 Term 和 Index，就能够表示一条唯一的日志

- Data：日志的具体内容，对应的是需要执行的指令



还有一个专门负责日志相关操作的结构体 RaftLog，如下：

```go
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot
}
```

RaftLog 有下面几个组成部分：

- storage：负责日志的持久化存储
- committed：当前已经提交的最大日志索引项

- applied：当前已经应用到状态机的最大日志索引项
- stabled：已经持久化保存的最大日志索引项

committed、applied、stabled 会随着节点日志状态的变化而改变，需要注意的是，只有日志提交了，才能被应用到状态机中，因此 applied <= committed。



RaftLog 的日志 分为了几个部分：



![img](https://cdn.nlark.com/yuque/0/2021/png/12925940/1637392411510-e5930a92-14ec-4364-96bd-b316a74d650f.png)

理解了以上概念，再来看看日志复制的流程：



日志复制的主要逻辑实现需要处理 MsgPropose、MsgAppend、MsgAppendResponse 这几类消息。

首先是 MsgPropose 消息，它是一个本地消息，将数据追加至 Leader 的日志中。然后需要向其他 Follower 节点广播消息，将日志同步到其他节点。

MsgAppend 消息需要携带的信息如下：

```go
		msg = pb.Message{
			MsgType: pb.MessageType_MsgAppend,
			From:    r.id,
			To:      to,
			Term:    r.Term,
			LogTerm: prevLogTerm,
			Index:   prevLogIndex,
			Entries: entries,
			Commit:  r.RaftLog.committed,
		}
```

发送到 Follower 节点之后，开始处理，主要的实现在 handleAppendEntries 方法当中，处理的时候需要注意这几点：

- 如果是任期号比自己低的消息，则直接拒绝
- 如果是任期号比自己高的消息，当前节点需要变更为 Follower

- 重置选举超时
- 检查消息的 Index 和 Term

- - 如果 msg 的 Index 比当前节点最后一条日志的索引大，则拒绝该条消息
  - 如果 msg 的 Index 所在的任期编号和 msg 的 prevLogTerm 不一致，则拒绝该条消息

- - 向 Leader 发送 MsgAppendResponse 消息

Leader 收到 Follower 的 MsgAppendResponse 消息后，需要处理：

- 如果 Follower 拒绝了这条消息，说明 Leader 节点和 Follower 的日志不一致，需要重新调整发送日志的 Index 和 Term，然后再向 Follower 发送 MsgAppend 消息
- 如果 Follower 接受了这条消息，Leader 需要判断是否有超过半数的节点接受了这条日志，如果是的话，则更新自己的 committed 信息，表示日志已经提交。