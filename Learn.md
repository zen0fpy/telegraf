1. tag、版本、提交、分支信息等,保存到程序方便定位问题
2. 懂得怎么控制进程生命周期 (启动，优雅退出)
   main(进程入口）-> Run -> ReLoadLoop(循环, 怎么参数, 这样子传好处，怎么用chan控制进程退出)
3. 程序加载循环(
   reload chan 控制配置加载 (如果支持动态加载时候，要先对配置格式校验，失败就不加载)
   Siganl 处理信号
   CancalContext + stop chan 控制进程退出
)
4. agent主流程(
   初始化加载配置 (
        配置支持多种加载模式
        设置配置中的一些参数
        配置检查时间间隔
        指标刷新时间间隔,
        初始化Inputs, Processor, Aggregator, Output
  )
  (支持配置校验功能)
  创建agent
  初始化日志 (支持切割, 归档)
  运行Agent.Run(ctx)
)


5. Agent结构设计(   
   多个Input构成一个InputUnit
   Aggregator还包含有AggProcessor
                                 *
            |---> Inputs ---------------> InputUnit                 |
            |                                 |                     |
            |                    *            V                     |
   Config ->|---> Processor ------------> ProcessorUnit             |
            |                                 |                   指标数据
            |                    *            V                     | 
            |---> Aggregator -----------> AggregatorUnit            |
            |                                 |                     |
            |                    *            V                     |
            |---> Outputs --------------> OutputUnit                V
)


6. Inputs、Processor、Aggregator、Outputs 接口设计
(
  InputInterface: {
     Gather()
  }
  ServiceInputInterface: {
     Input
     Start()
     Stop()
  }


  ProcessorInterface: {
  }

  AggregatorInterface: {
  }
  
  OutputsInterface: {
  }
)

8. 运行Agent(
   根据配置初始化插件 (工厂函数模式实现的)
   // 这个启动顺序很讲究的, 是采用流水处理模式
   // Outputs需要先启动，把的接收chan, 传给Aggregator,  
   // 它才能从Aggregator处接收数据
   启动Outputs -> Aggregator -> Processor -> Inputs
   分别启动一个goroutine执行Outputs、Aggregator、Processor、Inputs

   runOutput(
      for (
          go (wg)(
             ticker (
                for-select (
                    go(deal-metric) 
                  )
                )
            )
         )

   runAggregator(
     for (
        go (wg)(
           aggregator.Add(Metric)
        )
    )
   )

   runProcessor(
      for (
         go (wg)(
              processor.process(metric, Accumulator)
         )
      )
   )

   runInputs(
     for(
        go (wg) (
            for-select(
               go(deal-metric)
               )
            )
        )
     )
   )
)



#### 其它
3. 学会chan select for time goroutine使用, 怎么避免goroutine泄漏
4. 执行流程
5. 怎么打印日志，日志分类（Access, Error)
6. 怎么实现插件，加载插件（动态？）
7. 性能查询接口 (有没有必要激活, 临时开启用处大不大)
