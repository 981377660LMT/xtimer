package service

import (
	"context"
	"time"

	mconf "github.com/xiaoxuxiansheng/xtimer/common/conf"
	"github.com/xiaoxuxiansheng/xtimer/common/consts"
	"github.com/xiaoxuxiansheng/xtimer/common/utils"
	taskdao "github.com/xiaoxuxiansheng/xtimer/dao/task"
	timerdao "github.com/xiaoxuxiansheng/xtimer/dao/timer"
	"github.com/xiaoxuxiansheng/xtimer/pkg/cron"
	"github.com/xiaoxuxiansheng/xtimer/pkg/log"
	"github.com/xiaoxuxiansheng/xtimer/pkg/pool"
	"github.com/xiaoxuxiansheng/xtimer/pkg/redis"
)

type Worker struct {
	timerDAO          *timerdao.TimerDAO
	taskDAO           *taskdao.TaskDAO
	taskCache         *taskdao.TaskCache
	cronParser        *cron.CronParser
	lockService       *redis.Client
	appConfigProvider *mconf.MigratorAppConfProvider
	pool              pool.WorkerPool
}

func NewWorker(timerDAO *timerdao.TimerDAO, taskDAO *taskdao.TaskDAO, taskCache *taskdao.TaskCache, lockService *redis.Client,
	cronParser *cron.CronParser, appConfigProvider *mconf.MigratorAppConfProvider) *Worker {
	return &Worker{
		pool:              pool.NewGoWorkerPool(appConfigProvider.Get().WorkersNum),
		timerDAO:          timerDAO,
		taskDAO:           taskDAO,
		taskCache:         taskCache,
		lockService:       lockService,
		cronParser:        cronParser,
		appConfigProvider: appConfigProvider,
	}
}

func (w *Worker) Start(ctx context.Context) error {
	conf := w.appConfigProvider.Get()
	ticker := time.NewTicker(time.Duration(conf.MigrateStepMinutes) * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		log.InfoContext(ctx, "migrator ticking...")
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		locker := w.lockService.GetDistributionLock(utils.GetMigratorLockKey(utils.GetStartHour(time.Now())))
		if err := locker.Lock(ctx, int64(conf.MigrateTryLockMinutes)*int64(time.Minute/time.Second)); err != nil {
			log.ErrorContext(ctx, "migrator get lock failed, key: %s, err: %v", utils.GetMigratorLockKey(utils.GetStartHour(time.Now())), err)
			continue
		}

		if err := w.migrate(ctx); err != nil {
			log.ErrorContext(ctx, "migrate failed, err: %v", err)
			continue
		}

		_ = locker.ExpireLock(ctx, int64(conf.MigrateSucessExpireMinutes)*int64(time.Minute/time.Second))
	}
	return nil
}

func (w *Worker) migrate(ctx context.Context) error {
	// 获取所有激活状态的定时器
	timers, err := w.timerDAO.GetTimers(ctx, timerdao.WithStatus(int32(consts.Enabled.ToInt())))
	if err != nil {
		return err
	}
	// 设置时间窗口
	conf := w.appConfigProvider.Get()
	now := time.Now()
	start, end := utils.GetStartHour(now.Add(time.Duration(conf.MigrateStepMinutes)*time.Minute)), utils.GetStartHour(now.Add(2*time.Duration(conf.MigrateStepMinutes)*time.Minute))

	// 迁移可以慢慢来，不着急
	// 处理每个定时器，根据cron表达式生成具体任务
	for _, timer := range timers {
		// 计算在时间窗口内的执行时间点
		nexts, _ := w.cronParser.NextsBetween(timer.Cron, start, end)
		// 生成任务并保存到数据库
		if err := w.timerDAO.BatchCreateRecords(ctx, timer.BatchTasksFromTimer(nexts)); err != nil {
			log.ErrorContextf(ctx, "migrator batch create records for timer: %d failed, err: %v", timer.ID, err)
		}
		time.Sleep(5 * time.Second)
	}

	// if err := w.batchCreateBucket(ctx, start, end); err != nil {
	// 	log.ErrorContextf(ctx, "batch create bucket failed, start: %v", start)
	// 	return err
	// }

	// log.InfoContext(ctx, "migrator batch create db tasks susccess")
	// 将任务缓存到Redis
	return w.migrateToCache(ctx, start, end)
}

// func (w *Worker) batchCreateBucket(ctx context.Context, start, end time.Time) error {
// 	cntByMins, err := w.taskDAO.CountGroupByMinute(ctx, start.Format(consts.SecondFormat), end.Format(consts.SecondFormat))
// 	if err != nil {
// 		return err
// 	}

// 	return w.taskCache.BatchCreateBucket(ctx, cntByMins, end)
// }

func (w *Worker) migrateToCache(ctx context.Context, start, end time.Time) error {
	// 迁移完成后，将所有添加的 task 取出，添加到 redis 当中
	tasks, err := w.taskDAO.GetTasks(ctx, taskdao.WithStartTime(start), taskdao.WithEndTime(end))
	if err != nil {
		log.ErrorContextf(ctx, "migrator batch get tasks failed, err: %v", err)
		return err
	}
	// log.InfoContext(ctx, "migrator batch get tasks susccess")
	return w.taskCache.BatchCreateTasks(ctx, tasks, start, end)
}
