package services

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 命令回执超时时间：30 秒
const CommandAckTimeout = 30 * time.Second

var (
	ErrCommandNoConflict = errors.New("command number conflict")
	ErrCommandNotFound   = errors.New("command not found")
)

type CommandService struct {
	alertService *AlertService
}

func NewCommandService() *CommandService {
	return &CommandService{
		alertService: NewAlertService(),
	}
}

// generateCommandNo 生成命令编号：CMD + 时间戳 + 4 位随机字符
func generateCommandNo() string {
	const letters = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 4)
	rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return fmt.Sprintf("CMD%s%s", time.Now().Format("20060102150405"), string(b))
}

// isUniqueViolation 判断是否为唯一约束冲突（编号冲突）
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateCommand 生成待确认命令。编号冲突时重试，仍冲突则明确拒绝。
func (s *CommandService) CreateCommand(zoneID, deviceID uint, logID *uint) (*models.IrrigationCommand, error) {
	now := time.Now()

	var cmd *models.IrrigationCommand
	for attempt := 0; attempt < 3; attempt++ {
		cmd = &models.IrrigationCommand{
			CommandNo:       generateCommandNo(),
			ZoneID:          zoneID,
			DeviceID:        deviceID,
			IrrigationLogID: logID,
			Status:          models.CommandStatusPending,
			ExpiresAt:       now.Add(CommandAckTimeout),
		}
		err := database.DB.Create(cmd).Error
		if err == nil {
			return cmd, nil
		}
		if !isUniqueViolation(err) {
			return nil, err
		}
	}

	return nil, ErrCommandNoConflict
}

// GetCommandByNo 按编号查询命令
func (s *CommandService) GetCommandByNo(commandNo string) (*models.IrrigationCommand, error) {
	var cmd models.IrrigationCommand
	if err := database.DB.Where("command_no = ?", commandNo).First(&cmd).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCommandNotFound
		}
		return nil, err
	}
	return &cmd, nil
}

// CommandAckResult 单个回执的处理结果
type CommandAckResult struct {
	CommandNo string `json:"command_no"`
	Accepted  bool   `json:"accepted"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// ProcessAcks 处理设备心跳带回的命令回执。
// 只有 pending 且未过期的命令才会被置为 executing；
// 重复或过期回执不再改变状态，仅在结果中说明。
func (s *CommandService) ProcessAcks(deviceID uint, commandNos []string) []CommandAckResult {
	results := make([]CommandAckResult, 0, len(commandNos))
	now := time.Now()

	for _, no := range commandNos {
		result := CommandAckResult{CommandNo: no}

		cmd, err := s.GetCommandByNo(no)
		if err != nil {
			result.Reason = "unknown command number"
			results = append(results, result)
			continue
		}

		result.Status = string(cmd.Status)

		if cmd.DeviceID != deviceID {
			result.Reason = "command does not belong to this device"
			results = append(results, result)
			continue
		}

		if cmd.Status != models.CommandStatusPending {
			result.Reason = "duplicate ack, status unchanged"
			results = append(results, result)
			continue
		}

		if now.After(cmd.ExpiresAt) {
			result.Reason = "ack expired, status unchanged"
			results = append(results, result)
			continue
		}

		update := database.DB.Model(&models.IrrigationCommand{}).
			Where("command_no = ? AND status = ?", no, models.CommandStatusPending).
			Updates(map[string]interface{}{
				"status":   models.CommandStatusExecuting,
				"acked_at": now,
			})
		if update.Error != nil {
			result.Reason = update.Error.Error()
			results = append(results, result)
			continue
		}
		if update.RowsAffected == 0 {
			result.Reason = "command state changed concurrently, status unchanged"
			results = append(results, result)
			continue
		}

		result.Accepted = true
		result.Status = string(models.CommandStatusExecuting)
		results = append(results, result)
	}

	return results
}

// FailExpiredCommands 将超时未回执的待确认命令记为失败并生成告警。
// 返回失败的命令数量。
func (s *CommandService) FailExpiredCommands() (int, error) {
	now := time.Now()

	var expired []models.IrrigationCommand
	if err := database.DB.
		Where("status = ? AND expires_at < ?", models.CommandStatusPending, now).
		Find(&expired).Error; err != nil {
		return 0, err
	}

	failed := 0
	for _, cmd := range expired {
		reason := "device did not acknowledge command within 30s"
		result := database.DB.Model(&models.IrrigationCommand{}).
			Where("id = ? AND status = ?", cmd.ID, models.CommandStatusPending).
			Updates(map[string]interface{}{
				"status":         models.CommandStatusFailed,
				"failure_reason": reason,
			})
		if result.Error != nil || result.RowsAffected == 0 {
			continue
		}
		failed++

		if cmd.IrrigationLogID != nil {
			database.DB.Model(&models.IrrigationLog{}).
				Where("id = ? AND status = ?", *cmd.IrrigationLogID, models.ExecutionStatusInProgress).
				Updates(map[string]interface{}{
					"status":        models.ExecutionStatusFailed,
					"end_time":      now,
					"error_message": "command " + cmd.CommandNo + " not acknowledged by device",
				})
		}

		s.alertService.CreateIrrigationFailedAlert(&cmd.ZoneID,
			fmt.Sprintf("命令 %s 下发后 30 秒内未收到设备回执，灌溉执行失败", cmd.CommandNo))
	}

	return failed, nil
}
