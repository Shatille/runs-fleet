<#
.SYNOPSIS
    Windows bootstrap script for runs-fleet GitHub Actions runners.

.DESCRIPTION
    This script initializes a Windows Server instance as a GitHub Actions self-hosted runner.
    It downloads the runner agent, configures it with a JIT token, and starts the runner service.

.NOTES
    This script runs at instance boot via EC2 user data.
    Requires PowerShell 5.1+ and Windows Server 2022.
#>

param(
    [string]$RunnerVersion = "2.311.0"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# Configuration
$RunnerDir = "C:\actions-runner"
$AgentBinaryDir = "C:\runs-fleet\agent"
$LogFile = "C:\runs-fleet\bootstrap.log"
$IMDSToken = $null

function Write-Log {
    param([string]$Message)
    $timestamp = Get-Date -Format "yyyy-MM-dd HH:mm:ss"
    $logMessage = "$timestamp - $Message"
    Write-Host $logMessage
    Add-Content -Path $LogFile -Value $logMessage
}

function Get-IMDSToken {
    try {
        $response = Invoke-RestMethod -Uri "http://169.254.169.254/latest/api/token" `
            -Method PUT `
            -Headers @{"X-aws-ec2-metadata-token-ttl-seconds" = "21600"} `
            -TimeoutSec 5
        return $response
    } catch {
        Write-Log "Failed to get IMDS token: $_"
        throw
    }
}

function Get-InstanceMetadata {
    param([string]$Path)

    if (-not $IMDSToken) {
        $script:IMDSToken = Get-IMDSToken
    }

    try {
        $response = Invoke-RestMethod -Uri "http://169.254.169.254/latest/meta-data/$Path" `
            -Headers @{"X-aws-ec2-metadata-token" = $IMDSToken} `
            -TimeoutSec 5
        return $response
    } catch {
        Write-Log "Failed to get metadata $Path : $_"
        throw
    }
}

function Get-SSMParameter {
    param([string]$Name)

    try {
        $region = Get-InstanceMetadata -Path "placement/region"
        $result = aws ssm get-parameter --name $Name --with-decryption --region $region --output text --query "Parameter.Value"
        return $result
    } catch {
        Write-Log "Failed to get SSM parameter $Name : $_"
        throw
    }
}

function Install-GithubRunner {
    Write-Log "Installing GitHub Actions runner..."

    # Create runner directory
    if (Test-Path $RunnerDir) {
        Remove-Item -Path $RunnerDir -Recurse -Force
    }
    New-Item -ItemType Directory -Path $RunnerDir -Force | Out-Null

    # Download runner
    $runnerUrl = "https://github.com/actions/runner/releases/download/v$RunnerVersion/actions-runner-win-x64-$RunnerVersion.zip"
    $runnerZip = "$env:TEMP\actions-runner.zip"

    Write-Log "Downloading runner from $runnerUrl"
    Invoke-WebRequest -Uri $runnerUrl -OutFile $runnerZip -UseBasicParsing

    Write-Log "Extracting runner to $RunnerDir"
    Expand-Archive -Path $runnerZip -DestinationPath $RunnerDir -Force
    Remove-Item -Path $runnerZip -Force

    Write-Log "GitHub Actions runner installed successfully"
}

function Configure-Runner {
    param([string]$JitConfig)

    Write-Log "Configuring runner with JIT token..."

    Set-Location $RunnerDir

    # Write JIT config to file
    $jitConfigFile = "$RunnerDir\.runner_jit"
    Set-Content -Path $jitConfigFile -Value $JitConfig -NoNewline

    # Configure runner
    $configArgs = @(
        "--jitconfig", $jitConfigFile,
        "--unattended"
    )

    & "$RunnerDir\config.cmd" $configArgs
    if ($LASTEXITCODE -ne 0) {
        throw "Runner configuration failed with exit code $LASTEXITCODE"
    }

    # Clean up JIT config file
    Remove-Item -Path $jitConfigFile -Force -ErrorAction SilentlyContinue

    Write-Log "Runner configured successfully"
}

function Start-Runner {
    Write-Log "Starting GitHub Actions runner..."

    Set-Location $RunnerDir

    # Run the runner (blocking - will run until job completes)
    & "$RunnerDir\run.cmd" --once
    $exitCode = $LASTEXITCODE

    Write-Log "Runner exited with code $exitCode"
    return $exitCode
}

function Install-RunsFleetAgent {
    Write-Log "Installing runs-fleet agent..."

    # Create agent directory
    if (-not (Test-Path $AgentBinaryDir)) {
        New-Item -ItemType Directory -Path $AgentBinaryDir -Force | Out-Null
    }

    # Download agent from S3 (assumes agent binary is pre-staged)
    $region = Get-InstanceMetadata -Path "placement/region"
    $bucket = Get-SSMParameter -Name "/runs-fleet/config/agent-bucket"

    if ($bucket) {
        $agentKey = "agent/runs-fleet-agent-windows-amd64.exe"
        $agentPath = "$AgentBinaryDir\runs-fleet-agent.exe"

        Write-Log "Downloading agent from s3://$bucket/$agentKey"
        aws s3 cp "s3://$bucket/$agentKey" $agentPath --region $region

        if (Test-Path $agentPath) {
            Write-Log "Agent installed at $agentPath"
        }
    } else {
        Write-Log "No agent bucket configured, skipping agent install"
    }
}

function Send-Telemetry {
    param(
        [string]$Status,
        [int]$ExitCode,
        [int]$DurationSeconds
    )

    Write-Log "Sending telemetry: status=$Status, exitCode=$ExitCode, duration=$DurationSeconds"

    # Send to CloudWatch or other telemetry endpoint
    # Implementation depends on telemetry infrastructure
}

function Request-SelfTermination {
    Write-Log "Requesting self-termination..."

    $instanceId = Get-InstanceMetadata -Path "instance-id"
    $region = Get-InstanceMetadata -Path "placement/region"

    # Give some time for logs to flush
    Start-Sleep -Seconds 5

    # Terminate instance
    aws ec2 terminate-instances --instance-ids $instanceId --region $region
}

# Main execution
try {
    Write-Log "=== runs-fleet Windows Bootstrap Starting ==="
    $startTime = Get-Date

    # Get instance metadata
    $instanceId = Get-InstanceMetadata -Path "instance-id"
    $region = Get-InstanceMetadata -Path "placement/region"
    Write-Log "Instance: $instanceId in $region"

    # Get JIT config from SSM Parameter Store
    $jitParamName = "/runs-fleet/runner/$instanceId/jit-config"
    Write-Log "Fetching JIT config from $jitParamName"
    $jitConfig = Get-SSMParameter -Name $jitParamName

    if (-not $jitConfig) {
        throw "JIT config not found in SSM Parameter Store"
    }

    # Install and configure runner
    Install-GithubRunner
    Install-RunsFleetAgent
    Configure-Runner -JitConfig $jitConfig

    # Start runner and wait for job completion
    $exitCode = Start-Runner

    $endTime = Get-Date
    $duration = [int]($endTime - $startTime).TotalSeconds

    # Send telemetry
    $status = if ($exitCode -eq 0) { "success" } else { "failure" }
    Send-Telemetry -Status $status -ExitCode $exitCode -DurationSeconds $duration

    Write-Log "=== Bootstrap Complete: $status (duration: ${duration}s) ==="

} catch {
    Write-Log "FATAL ERROR: $_"
    Write-Log $_.ScriptStackTrace

    Send-Telemetry -Status "error" -ExitCode 1 -DurationSeconds 0

} finally {
    # Always terminate the instance
    Request-SelfTermination
}
