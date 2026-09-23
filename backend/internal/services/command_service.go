package services

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// CommandAckTimeout 待确认命令的回执超时时间
const CommandAckTimeout = 30 * time.Second

// 命令下发/回执过程中的可预期错误，控制器据此返回明确的拒绝原因
var (
	ErrZoneNotFound         = errors.New("zone not found")
	ErrNoValveInZone        = errors.New("zone has no valve device")
	ErrMultipleValvesInZone = errors.New("zone has multiple valve devices, specify device_id")
	ErrDeviceNotFound       = errors.New("device not found")
	ErrDeviceOffline        = errors.New("device is offline")
	ErrDeviceZoneMismatch   = errors.New("device does not belong to the zone")
	ErrCommandNoConflict    = errors.New("command number conflict")
	ErrCommandNotFound      = errors.New("command not found")
)

type CommandService struct {
	alertService *AlertService
}

func NewCommandService() *CommandService {
	return &CommandService{
		alertService: NewAlertService(),
	}
}

// IssueManualCommandInput 手动浇水下发参数
type IssueManualCommandInput struct {
	ZoneID          uint
	DeviceID        *uint
	DurationSeconds int
	// CommandNo 由调用方（管理员）提供的幂等编号；为空则服务端生成
	CommandNo string
}

// IssueManualCommand 校验区域与阀门后生成一条 pending 命令及关联灌溉日志，返回命令编号。
// 离线设备、编号冲突、区域/设备不存在均在此明确拒绝。
func (s *CommandService) IssueManualCommand(input *IssueManualCommandInput) (*models.DeviceCommand, error) {
	// 1. 区域必须存在
	var zone models.IrrigationZone
	if err := database.DB.First(&zone, input.ZoneID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrZoneNotFound
		}
		return nil, err
	}

	// 2. 定位目标阀门：指定了设备就校验归属，未指定则要求区域内恰有一台阀门
	var device models.Device
	if input.DeviceID != nil {
		if err := database.DB.First(&device, *input.DeviceID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrDeviceNotFound
			}
			return nil, err
		}
		if device.ZoneID == nil || *device.ZoneID != input.ZoneID {
			return nil, ErrDeviceZoneMismatch
		}
		if device.Type != models.DeviceTypeValve {
			return nil, fmt.Errorf("device %d is not a valve", device.ID)
		}
	} else {
		var valves []models.Device
		if err := database.DB.Where("zone_id = ? AND type = ? AND deleted_at IS NULL", input.ZoneID, models.DeviceTypeValve).
			Find(&valves).Error; err != nil {
			return nil, err
		}
		if len(valves) == 0 {
			return nil, ErrNoValveInZone
		}
		if len(valves) > 1 {
			return nil, ErrMultipleValvesInZone
		}
		device = valves[0]
	}

	// 3. 离线设备直接拒绝（online/error 之外一律视为离线）
	if device.Status != models.DeviceStatusOnline {
		return nil, ErrDeviceOffline
	}

	duration := input.DurationSeconds
	if duration <= 0 {
		duration = 300
	}
	now := time.Now()

	// 4. 调用方自带编号时先查重，冲突明确拒绝
	commandNo := input.CommandNo
	if commandNo != "" {
		var count int64
		if err := database.DB.Model(&models.DeviceCommand{}).
			Where("command_no = ?", commandNo).Count(&count).Error; err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, ErrCommandNoConflict
		}
	} else {
		commandNo = generateCommandNo()
	}

	command := &models.DeviceCommand{
		CommandNo: commandNo,
		DeviceID:  device.ID,
		ZoneID:    input.ZoneID,
		Type:      models.CommandTypeOpenValve,
		Payload: map[string]interface{}{
			"duration_seconds": duration,
		},
		Status:    models.CommandStatusPending,
		ExpiresAt: now.Add(CommandAckTimeout),
	}

	// 5. 命令与灌溉日志同事务落库
	err := database.DB.Transaction(func(tx *gorm.DB) error {
		log := &models.IrrigationLog{
			ZoneID:      &input.ZoneID,
			TriggerType: models.TriggerTypeManual,
			StartTime:   now,
			Status:      models.ExecutionStatusInProgress,
		}
		if err := tx.Create(log).Error; err != nil {
			return err
		}

		command.IrrigationLogID = &log.ID
		if err := tx.Create(command).Error; err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrCommandNoConflict
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return command, nil
}

// AckCommand 处理设备心跳带回的命令编号：仅当编号属于该设备、仍处于 pending
// 且未过期时转为 executing。重复回执、过期回执、串设备回执均不再改状态，
// 返回 false。
func (s *CommandService) AckCommand(serial, commandNo string, ackedAt time.Time) (bool, error) {
	device, err := NewDeviceService().GetDeviceBySerial(serial)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrDeviceNotFound
		}
		return false, err
	}

	var command models.DeviceCommand
	if err := database.DB.Where("command_no = ?", commandNo).First(&command).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// 未知编号：可能是过期已清理的回执，忽略不改状态
			return false, nil
		}
		return false, err
	}

	if command.DeviceID != device.ID {
		// 编号属于其他设备，拒绝改状态
		return false, nil
	}

	result := database.DB.Model(&models.DeviceCommand{}).
		Where("command_no = ? AND status = ? AND expires_at > ?",
			commandNo, models.CommandStatusPending, ackedAt).
		Updates(map[string]interface{}{
			"status":   models.CommandStatusExecuting,
			"acked_at": ackedAt,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// ReportCommandResult 设备心跳回报命令执行结果。只接受 executing 状态的命令；
// pending 未过期先补确认再落终态；已失败/已成功的重复回报以及过期命令忽略。
func (s *CommandService) ReportCommandResult(serial, commandNo string, success bool, waterUsage *float64, errorMsg *string, reportedAt time.Time) (bool, error) {
	device, err := NewDeviceService().GetDeviceBySerial(serial)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrDeviceNotFound
		}
		return false, err
	}

	var command models.DeviceCommand
	if err := database.DB.Where("command_no = ?", commandNo).First(&command).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}

	if command.DeviceID != device.ID {
		return false, nil
	}

	// 终态命令的重复回报、已超时失败的命令：忽略
	if command.Status == models.CommandStatusSuccess || command.Status == models.CommandStatusFailed {
		return false, nil
	}
	if command.Status == models.CommandStatusPending && !command.ExpiresAt.After(reportedAt) {
		return false, nil
	}

	targetStatus := models.CommandStatusSuccess
	if !success {
		targetStatus = models.CommandStatusFailed
	}

	updated := false
	err = database.DB.Transaction(func(tx *gorm.DB) error {
		updates := map[string]interface{}{"status": targetStatus}
		if !success && errorMsg != nil {
			updates["error_message"] = *errorMsg
		}
		result := tx.Model(&models.DeviceCommand{}).
			Where("command_no = ? AND status IN ? AND expires_at > ?",
				commandNo,
				[]models.CommandStatus{models.CommandStatusPending, models.CommandStatusExecuting},
				reportedAt).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		updated = true

		if command.IrrigationLogID != nil {
			logUpdates := map[string]interface{}{
				"end_time": reportedAt,
				"status":   statusToExecutionStatus(targetStatus),
				"duration": int(reportedAt.Sub(command.CreatedAt).Seconds()),
			}
			if waterUsage != nil {
				logUpdates["water_usage"] = *waterUsage
			}
			if !success && errorMsg != nil {
				logUpdates["error_message"] = *errorMsg
			}
			if err := tx.Model(&models.IrrigationLog{}).
				Where("id = ? AND status = ?", *command.IrrigationLogID, models.ExecutionStatusInProgress).
				Updates(logUpdates).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	if updated && !success {
		msg := fmt.Sprintf("阀门执行命令 %s 失败", commandNo)
		if errorMsg != nil && *errorMsg != "" {
			msg += ": " + *errorMsg
		}
		_ = s.alertService.CreateCommandAckTimeoutAlert(commandNo, device.ID, command.ZoneID, msg)
	}

	return updated, nil
}

// GetCommandByNo 管理员按编号查询命令结果
func (s *CommandService) GetCommandByNo(commandNo string) (*models.DeviceCommand, error) {
	var command models.DeviceCommand
	if err := database.DB.Where("command_no = ?", commandNo).First(&command).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCommandNotFound
		}
		return nil, err
	}
	return &command, nil
}

// ListPendingForDevice 返回设备尚未确认、仍在回执有效期内的命令，由心跳响应下发
func (s *CommandService) ListPendingForDevice(deviceID uint, now time.Time) ([]models.DeviceCommand, error) {
	var commands []models.DeviceCommand
	err := database.DB.
		Where("device_id = ? AND status = ? AND expires_at > ?",
			deviceID, models.CommandStatusPending, now).
		Order("created_at ASC").
		Find(&commands).Error
	return commands, err
}

// SweepExpiredCommands 将超过回执时限仍处于 pending 的命令标记为失败，
// 同步关联灌溉日志为失败并生成告警。由调度器周期性调用。
func (s *CommandService) SweepExpiredCommands(now time.Time) (int64, error) {
	var expired []models.DeviceCommand
	if err := database.DB.
		Where("status = ? AND expires_at <= ?", models.CommandStatusPending, now).
		Find(&expired).Error; err != nil {
		return 0, err
	}

	var failed int64
	for _, command := range expired {
		markedFailed := false
		err := database.DB.Transaction(func(tx *gorm.DB) error {
			errMsg := fmt.Sprintf("命令 %s 下发后 %d 秒内未收到设备回执", command.CommandNo, int(CommandAckTimeout.Seconds()))
			result := tx.Model(&models.DeviceCommand{}).
				Where("id = ? AND status = ?", command.ID, models.CommandStatusPending).
				Updates(map[string]interface{}{
					"status":        models.CommandStatusFailed,
					"error_message": errMsg,
				})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				// 并发的心跳回执已改状态，跳过
				return nil
			}
			markedFailed = true
			failed++

			if command.IrrigationLogID != nil {
				if err := tx.Model(&models.IrrigationLog{}).
					Where("id = ? AND status = ?", *command.IrrigationLogID, models.ExecutionStatusInProgress).
					Updates(map[string]interface{}{
						"end_time":      now,
						"status":        models.ExecutionStatusFailed,
						"error_message": errMsg,
					}).Error; err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return failed, err
		}

		if markedFailed {
			_ = s.alertService.CreateCommandAckTimeoutAlert(
				command.CommandNo, command.DeviceID, command.ZoneID,
				fmt.Sprintf("命令 %s 下发后 %d 秒内未收到设备回执，已判定失败",
					command.CommandNo, int(CommandAckTimeout.Seconds())),
			)
		}
	}

	return failed, nil
}

func generateCommandNo() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("CMD%s%s", time.Now().Format("20060102150405"), hex.EncodeToString(b))
}

func statusToExecutionStatus(status models.CommandStatus) models.ExecutionStatus {
	if status == models.CommandStatusSuccess {
		return models.ExecutionStatusSuccess
	}
	return models.ExecutionStatusFailed
}
