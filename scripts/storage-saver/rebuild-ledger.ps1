#requires -Version 5.1

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$ImmichUrl,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$StateDirectory,

    [Parameter(Mandatory = $true)]
    [switch]$Rebuild,

    [Parameter(Mandatory = $true)]
    [switch]$OptimizerStopped,

    [switch]$Merge,

    [ValidateRange(1, 1000)]
    [int]$PageSize = 500,

    [ValidateRange(1, 3600)]
    [int]$TimeoutSeconds = 120,

    [ValidateNotNullOrEmpty()]
    [string]$ApiKeyEnvironmentVariable = 'IUO_IMMICH_API_KEY'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$script:ProvenanceKey = 'storage-saver-source'
$script:LedgerVersion = 1
$script:HashPattern = '^[0-9a-f]{40}$'
$script:RecordProperties = @(
    'owner_id', 'asset_id', 'source_sha1', 'output_sha1', 'source_filename',
    'output_filename', 'preset', 'state', 'spool_path', 'source_size',
    'output_size', 'updated_at'
)

function Get-RequiredProperty {
    param(
        [Parameter(Mandatory = $true)] [object]$Object,
        [Parameter(Mandatory = $true)] [string]$Name,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    if ($null -eq $Object) {
        throw "$Context is null"
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        throw "$Context is missing '$Name'"
    }
    return $property.Value
}

function Get-OptionalProperty {
    param(
        [Parameter(Mandatory = $true)] [object]$Object,
        [Parameter(Mandatory = $true)] [string]$Name
    )

    if ($null -eq $Object) {
        return $null
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return $property.Value
}

function Assert-Hash {
    param(
        [Parameter(Mandatory = $true)] [string]$Value,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    if ($Value -cnotmatch $script:HashPattern) {
        throw "$Context must be 40 lowercase hexadecimal characters"
    }
    return $Value
}

function ConvertTo-UtcTimestamp {
    param(
        [Parameter(Mandatory = $true)] [object]$Value,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    $parsed = [DateTimeOffset]::MinValue
    if (-not [DateTimeOffset]::TryParse(
        [string]$Value,
        [Globalization.CultureInfo]::InvariantCulture,
        [Globalization.DateTimeStyles]::RoundtripKind,
        [ref]$parsed
    )) {
        throw "$Context is not a valid timestamp"
    }
    return $parsed.ToUniversalTime().ToString(
        "yyyy-MM-dd'T'HH:mm:ss.FFFFFFF'Z'",
        [Globalization.CultureInfo]::InvariantCulture
    )
}

function ConvertTo-Int64 {
    param(
        [Parameter(Mandatory = $true)] [object]$Value,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    $parsed = [Int64]0
    if (-not [Int64]::TryParse([string]$Value, [ref]$parsed) -or $parsed -lt 0) {
        throw "$Context must be a non-negative integer"
    }
    return $parsed
}

function Assert-StateDirectoryOutsideRepository {
    $repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
    $resolvedStateRoot = [IO.Path]::GetFullPath($StateDirectory)
    $comparison = if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
        [StringComparison]::OrdinalIgnoreCase
    } else {
        [StringComparison]::Ordinal
    }
    $rootPrefix = $repositoryRoot.TrimEnd([IO.Path]::DirectorySeparatorChar, [IO.Path]::AltDirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar

    if ($resolvedStateRoot.Equals($repositoryRoot, $comparison) -or
        $resolvedStateRoot.StartsWith($rootPrefix, $comparison)) {
        throw 'StateDirectory must not be inside the repository; generated ledgers belong on persistent storage'
    }
    return $resolvedStateRoot
}

function Assert-OptimizerStopped {
    if (-not $OptimizerStopped) {
        throw 'Explicit -OptimizerStopped confirmation is required'
    }

    $runningProcesses = @()
    $getCimInstance = Get-Command Get-CimInstance -ErrorAction SilentlyContinue
    if ($null -ne $getCimInstance) {
        try {
            $runningProcesses = @(Get-CimInstance Win32_Process -ErrorAction Stop | Where-Object {
                $_.ProcessId -ne $PID -and (
                    $_.Name -match '^(immich-optimizer|optimizer)(\.exe)?$' -or
                    ($_.CommandLine -and $_.CommandLine -match '(?i)(?:^|\s|[\\/])(immich-optimizer|cmd[\\/]optimizer|optimizer)(\.exe)?(?:\s|$)')
                )
            })
        } catch {
            Write-Verbose "Local process inspection was unavailable: $($_.Exception.Message)"
        }
    }

    if ($runningProcesses.Count -gt 0) {
        $description = ($runningProcesses | ForEach-Object { "$($_.Name)[$($_.ProcessId)]" }) -join ', '
        throw "Immich optimizer process appears to be running: $description"
    }

    $docker = Get-Command docker -ErrorAction SilentlyContinue
    if ($null -ne $docker) {
        $dockerRows = @(& docker ps --format '{{.ID}}|{{.Names}}|{{.Image}}|{{.Command}}' 2>$null)
        if ($LASTEXITCODE -eq 0 -and ($dockerRows -match 'immich-optimizer')) {
            throw 'An immich-optimizer Docker container appears to be running'
        }
    }
}

function Get-BaseImmichUrl {
    $uri = $null
    if (-not [Uri]::TryCreate($ImmichUrl, [UriKind]::Absolute, [ref]$uri)) {
        throw 'ImmichUrl must be an absolute URL'
    }
    if ($uri.Scheme -notin @('http', 'https')) {
        throw 'ImmichUrl must use HTTP or HTTPS'
    }
    if ($uri.Query -or $uri.Fragment) {
        throw 'ImmichUrl must not contain a query string or fragment'
    }
    return $uri.GetLeftPart([UriPartial]::Path).TrimEnd('/')
}

function Invoke-ImmichJson {
    param(
        [Parameter(Mandatory = $true)] [string]$Method,
        [Parameter(Mandatory = $true)] [string]$Path,
        [AllowNull()] [object]$Body = $null
    )

    $request = @{
        Uri         = $script:BaseImmichUrl + $Path
        Method      = $Method
        Headers     = $script:ApiHeaders
        TimeoutSec  = $TimeoutSeconds
        UseBasicParsing = $true
        ErrorAction = 'Stop'
    }
    if ($null -ne $Body) {
        $request.ContentType = 'application/json'
        $request.Body = $Body | ConvertTo-Json -Depth 8 -Compress
    }

    try {
        return Invoke-RestMethod @request
    } catch {
        $status = ''
        if ($null -ne $_.Exception.Response) {
            $status = " HTTP $([int]$_.Exception.Response.StatusCode)"
        }
        throw "Immich $Method $Path failed${status}: $($_.Exception.Message)"
    }
}

function Assert-ExactProperties {
    param(
        [Parameter(Mandatory = $true)] [object]$Object,
        [Parameter(Mandatory = $true)] [string[]]$Expected,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    if ($null -eq $Object) {
        throw "$Context is null"
    }
    $actual = @($Object.PSObject.Properties.Name)
    foreach ($name in $Expected) {
        if ($actual -notcontains $name) {
            throw "$Context is missing '$name'"
        }
    }
    foreach ($name in $actual) {
        if ($Expected -notcontains $name) {
            throw "$Context contains unsupported property '$name'"
        }
    }
}

function ConvertTo-ValidatedLedgerRecord {
    param(
        [Parameter(Mandatory = $true)] [object]$Record,
        [Parameter(Mandatory = $true)] [string]$Context
    )

    Assert-ExactProperties -Object $Record -Expected $script:RecordProperties -Context $Context

    $ownerID = Get-RequiredProperty $Record 'owner_id' $Context
    $assetID = Get-RequiredProperty $Record 'asset_id' $Context
    $sourceSHA1 = Get-RequiredProperty $Record 'source_sha1' $Context
    $outputSHA1 = Get-RequiredProperty $Record 'output_sha1' $Context
    $sourceFilename = Get-RequiredProperty $Record 'source_filename' $Context
    $outputFilename = Get-RequiredProperty $Record 'output_filename' $Context
    $preset = Get-RequiredProperty $Record 'preset' $Context
    $state = Get-RequiredProperty $Record 'state' $Context
    $spoolPath = Get-RequiredProperty $Record 'spool_path' $Context

    foreach ($value in @($ownerID, $assetID, $sourceSHA1, $outputSHA1, $sourceFilename, $outputFilename, $preset, $state, $spoolPath)) {
        if ($value -isnot [string]) {
            throw "$Context contains a non-string field"
        }
    }
    if ([string]::IsNullOrWhiteSpace($ownerID)) {
        throw "$Context owner_id is required"
    }
    [void](Assert-Hash ([string]$sourceSHA1) "$Context source_sha1")
    if (-not [string]::IsNullOrEmpty([string]$outputSHA1)) {
        [void](Assert-Hash ([string]$outputSHA1) "$Context output_sha1")
    }
    if ($state -notin @('staged', 'uploaded', 'verified', 'failed')) {
        throw "$Context has invalid state '$state'"
    }
    if ($state -in @('uploaded', 'verified')) {
        if ([string]::IsNullOrEmpty([string]$assetID)) {
            throw "$Context asset_id is required for state '$state'"
        }
        if ([string]::IsNullOrEmpty([string]$outputSHA1)) {
            throw "$Context output_sha1 is required for state '$state'"
        }
    }

    $sourceSize = ConvertTo-Int64 (Get-RequiredProperty $Record 'source_size' $Context) "$Context source_size"
    $outputSize = ConvertTo-Int64 (Get-RequiredProperty $Record 'output_size' $Context) "$Context output_size"
    $updatedAt = ConvertTo-UtcTimestamp (Get-RequiredProperty $Record 'updated_at' $Context) "$Context updated_at"

    return [pscustomobject][ordered]@{
        owner_id        = [string]$ownerID
        asset_id        = [string]$assetID
        source_sha1     = [string]$sourceSHA1
        output_sha1     = [string]$outputSHA1
        source_filename = [string]$sourceFilename
        output_filename = [string]$outputFilename
        preset          = [string]$preset
        state           = [string]$state
        spool_path      = [string]$spoolPath
        source_size     = $sourceSize
        output_size     = $outputSize
        updated_at      = $updatedAt
    }
}

function Read-ExistingLedger {
    param([Parameter(Mandatory = $true)] [string]$Path)

    $content = [IO.File]::ReadAllText($Path, [Text.UTF8Encoding]::new($false, $true))
    try {
        $ledger = $content | ConvertFrom-Json -ErrorAction Stop
    } catch {
        throw "Existing ledger is not valid JSON; refusing to modify it: $($_.Exception.Message)"
    }

    Assert-ExactProperties -Object $ledger -Expected @('version', 'records') -Context 'Existing ledger'
    if ([int](Get-RequiredProperty $ledger 'version' 'Existing ledger') -ne $script:LedgerVersion) {
        throw 'Existing ledger uses an unsupported schema version'
    }

    $records = @()
    $rawRecords = Get-RequiredProperty $ledger 'records' 'Existing ledger'
    $index = 0
    foreach ($record in @($rawRecords)) {
        $records += ConvertTo-ValidatedLedgerRecord $record "Existing ledger record $index"
        $index++
    }
    return $records
}

function Write-AtomicLedger {
    param(
        [Parameter(Mandatory = $true)] [string]$Path,
        [Parameter(Mandatory = $true)] [object[]]$Records
    )

    $sorted = @($Records | Sort-Object -Property owner_id, source_sha1)
    $ledger = [pscustomobject][ordered]@{
        version = $script:LedgerVersion
        records = @($sorted)
    }
    $json = ($ledger | ConvertTo-Json -Depth 8) + "`n"
    $bytes = [Text.UTF8Encoding]::new($false).GetBytes($json)
    $temporaryPath = Join-Path (Split-Path -Parent $Path) ('.ledger.json.rebuild-' + $PID + '-' + [Guid]::NewGuid().ToString('N') + '.tmp')

    try {
        $stream = [IO.FileStream]::new(
            $temporaryPath,
            [IO.FileMode]::CreateNew,
            [IO.FileAccess]::Write,
            [IO.FileShare]::None
        )
        try {
            $stream.Write($bytes, 0, $bytes.Length)
            $stream.Flush($true)
        } finally {
            $stream.Dispose()
        }

        if ([IO.File]::Exists($Path)) {
            [IO.File]::Replace($temporaryPath, $Path, $null)
        } else {
            [IO.File]::Move($temporaryPath, $Path)
        }
    } finally {
        if ([IO.File]::Exists($temporaryPath)) {
            Remove-Item -LiteralPath $temporaryPath -Force
        }
    }
}

function Get-AuthenticatedOwner {
    $user = Invoke-ImmichJson -Method 'GET' -Path '/api/users/me'
    $ownerID = [string](Get-RequiredProperty $user 'id' 'Authenticated user')
    if ([string]::IsNullOrWhiteSpace($ownerID)) {
        throw 'Authenticated user response has no id'
    }
    return $ownerID
}

function Get-AllAssetIDs {
    $page = 1
    $seenPages = @{}
    $seenAssetIDs = @{}
    $assetIDs = [Collections.Generic.List[string]]::new()
    $uuidPattern = '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'

    while ($true) {
        if ($seenPages.ContainsKey([string]$page)) {
            throw "Search pagination repeated page $page"
        }
        $seenPages[[string]$page] = $true

        $response = Invoke-ImmichJson -Method 'POST' -Path '/api/search/metadata' -Body ([ordered]@{
            page = $page
            size = $PageSize
        })
        $assets = Get-RequiredProperty $response 'assets' 'Search response'
        $items = @((Get-RequiredProperty $assets 'items' 'Search response assets'))
        foreach ($item in $items) {
            $assetID = [string](Get-RequiredProperty $item 'id' 'Search asset')
            if ($assetID -notmatch $uuidPattern) {
                throw "Search returned an invalid asset id '$assetID'"
            }
            if (-not $seenAssetIDs.ContainsKey($assetID)) {
                $seenAssetIDs[$assetID] = $true
                $assetIDs.Add($assetID)
            }
        }

        $nextPageValue = Get-OptionalProperty $assets 'nextPage'
        if ([string]::IsNullOrWhiteSpace([string]$nextPageValue)) {
            break
        }
        $nextPage = 0
        if (-not [int]::TryParse([string]$nextPageValue, [ref]$nextPage) -or $nextPage -le $page) {
            throw "Search returned an invalid nextPage value '$nextPageValue'"
        }
        if ($items.Count -eq 0) {
            throw 'Search returned a next page without any assets'
        }
        $page = $nextPage
    }

    return $assetIDs.ToArray()
}

function Get-ProvenanceEntry {
    param([Parameter(Mandatory = $true)] [string]$AssetID)

    $escapedID = [Uri]::EscapeDataString($AssetID)
    $metadata = @(Invoke-ImmichJson -Method 'GET' -Path "/api/assets/$escapedID/metadata")
    $matches = @($metadata | Where-Object {
        [string](Get-OptionalProperty $_ 'key') -ceq $script:ProvenanceKey
    })

    if ($matches.Count -eq 0) {
        return $null
    }
    if ($matches.Count -ne 1) {
        throw "Asset $AssetID has duplicate '$script:ProvenanceKey' metadata keys"
    }

    $value = Get-RequiredProperty $matches[0] 'value' "Asset $AssetID provenance"
    $updatedAt = ConvertTo-UtcTimestamp (Get-RequiredProperty $matches[0] 'updatedAt' "Asset $AssetID provenance") "Asset $AssetID provenance updatedAt"
    return [pscustomobject]@{
        Value     = $value
        UpdatedAt = $updatedAt
    }
}

function ConvertTo-ProvenanceCandidate {
    param(
        [Parameter(Mandatory = $true)] [string]$AssetID,
        [Parameter(Mandatory = $true)] [object]$Entry,
        [Parameter(Mandatory = $true)] [string]$AuthenticatedOwner
    )

    $value = Get-RequiredProperty $Entry 'Value' "Asset $AssetID provenance"
    $schemaVersion = ConvertTo-Int64 (Get-RequiredProperty $value 'schemaVersion' "Asset $AssetID provenance") "Asset $AssetID schemaVersion"
    if ($schemaVersion -ne 1) {
        throw "Asset $AssetID uses unsupported provenance schemaVersion $schemaVersion"
    }

    $ownerID = [string](Get-RequiredProperty $value 'ownerId' "Asset $AssetID provenance")
    if ($ownerID -cne $AuthenticatedOwner) {
        throw "Asset $AssetID provenance owner does not match the authenticated owner"
    }

    $sourceSHA1 = Assert-Hash ([string](Get-RequiredProperty $value 'sourceSHA1' "Asset $AssetID provenance")) "Asset $AssetID sourceSHA1"
    $outputSHA1 = Assert-Hash ([string](Get-RequiredProperty $value 'outputSHA1' "Asset $AssetID provenance")) "Asset $AssetID outputSHA1"
    $sourceFilename = [string](Get-RequiredProperty $value 'sourceFilename' "Asset $AssetID provenance")
    $outputFilename = [string](Get-RequiredProperty $value 'outputFilename' "Asset $AssetID provenance")
    if ([string]::IsNullOrWhiteSpace($sourceFilename) -or [string]::IsNullOrWhiteSpace($outputFilename)) {
        throw "Asset $AssetID provenance is missing a filename"
    }

    $preset = [string](Get-RequiredProperty $value 'preset' "Asset $AssetID provenance")
    $sourceSize = ConvertTo-Int64 (Get-RequiredProperty $value 'sourceSize' "Asset $AssetID provenance") "Asset $AssetID sourceSize"
    $outputSize = ConvertTo-Int64 (Get-RequiredProperty $value 'outputSize' "Asset $AssetID provenance") "Asset $AssetID outputSize"
    $updatedAt = [string](Get-RequiredProperty $Entry 'UpdatedAt' "Asset $AssetID provenance")

    return [pscustomobject]@{
        OwnerID        = $ownerID
        AssetID        = $AssetID
        SourceSHA1     = $sourceSHA1
        OutputSHA1     = $outputSHA1
        SourceFilename = $sourceFilename
        OutputFilename = $outputFilename
        Preset         = $preset
        SourceSize     = $sourceSize
        OutputSize     = $outputSize
        UpdatedAt      = $updatedAt
    }
}

function Get-RemoteOriginalInfo {
    param([Parameter(Mandatory = $true)] [string]$AssetID)

    $escapedID = [Uri]::EscapeDataString($AssetID)
    $url = $script:BaseImmichUrl + "/api/assets/$escapedID/original"
    $temporaryPath = Join-Path ([IO.Path]::GetTempPath()) ('immich-optimizer-output-' + [Guid]::NewGuid().ToString('N') + '.bin')
    try {
        try {
            Invoke-WebRequest -Uri $url -Method 'GET' -Headers $script:ApiHeaders -OutFile $temporaryPath -UseBasicParsing -TimeoutSec $TimeoutSeconds -ErrorAction Stop | Out-Null
        } catch {
            throw "Immich original download failed for asset ${AssetID}: $($_.Exception.Message)"
        }
        $hash = (Get-FileHash -LiteralPath $temporaryPath -Algorithm SHA1).Hash.ToLowerInvariant()
        $size = (Get-Item -LiteralPath $temporaryPath).Length
        return [pscustomobject]@{
            SHA1 = $hash
            Size = [Int64]$size
        }
    } finally {
        if ([IO.File]::Exists($temporaryPath)) {
            Remove-Item -LiteralPath $temporaryPath -Force
        }
    }
}

function Get-RecordKey {
    param(
        [Parameter(Mandatory = $true)] [string]$OwnerID,
        [Parameter(Mandatory = $true)] [string]$SourceSHA1
    )
    return $OwnerID + [char]0 + $SourceSHA1
}

function Merge-VerifiedRecord {
    param(
        [AllowNull()] [object]$Existing,
        [Parameter(Mandatory = $true)] [object]$Recovered
    )

    $sourceFilename = $Recovered.SourceFilename
    $outputFilename = $Recovered.OutputFilename
    $preset = $Recovered.Preset
    $spoolPath = ''

    if ($null -ne $Existing) {
        if (-not [string]::IsNullOrEmpty($Existing.asset_id) -and $Existing.asset_id -cne $Recovered.AssetID) {
            throw "Asset alias conflict for source $($Recovered.SourceSHA1): existing asset id differs from verified metadata"
        }
        if (-not [string]::IsNullOrEmpty($Existing.output_sha1) -and $Existing.output_sha1 -cne $Recovered.OutputSHA1) {
            throw "Output hash conflict for source $($Recovered.SourceSHA1): existing record differs from verified output"
        }
        if (-not [string]::IsNullOrWhiteSpace($Existing.source_filename)) {
            $sourceFilename = $Existing.source_filename
        }
        if (-not [string]::IsNullOrWhiteSpace($Existing.output_filename)) {
            $outputFilename = $Existing.output_filename
        }
        if (-not [string]::IsNullOrWhiteSpace($Existing.preset)) {
            $preset = $Existing.preset
        }
        $spoolPath = $Existing.spool_path
    }

    return [pscustomobject][ordered]@{
        owner_id        = $Recovered.OwnerID
        asset_id        = $Recovered.AssetID
        source_sha1     = $Recovered.SourceSHA1
        output_sha1     = $Recovered.OutputSHA1
        source_filename = $sourceFilename
        output_filename = $outputFilename
        preset          = $preset
        state           = 'verified'
        spool_path      = $spoolPath
        source_size     = $Recovered.SourceSize
        output_size     = $Recovered.OutputSize
        updated_at      = $Recovered.UpdatedAt
    }
}

function Invoke-Rebuild {
    if (-not $Rebuild) {
        throw 'Explicit -Rebuild invocation is required'
    }

    # This script is the only ledger writer. Running it while the optimizer is
    # active can lose records when the parent later persists its in-memory copy.
    Assert-OptimizerStopped

    $resolvedStateDirectory = Assert-StateDirectoryOutsideRepository
    if (-not [IO.Directory]::Exists($resolvedStateDirectory)) {
        [void][IO.Directory]::CreateDirectory($resolvedStateDirectory)
    }
    $ledgerPath = Join-Path $resolvedStateDirectory 'ledger.json'
    $ledgerExists = [IO.File]::Exists($ledgerPath)
    if ($ledgerExists -and -not $Merge) {
        throw "Existing ledger found at the configured state path. Refusing to replace it without explicit -Merge."
    }

    $existingRecords = @()
    if ($ledgerExists) {
        $existingRecords = @(Read-ExistingLedger -Path $ledgerPath)
    }

    $apiKey = [Environment]::GetEnvironmentVariable($ApiKeyEnvironmentVariable)
    if ([string]::IsNullOrWhiteSpace($apiKey)) {
        throw "Required environment variable '$ApiKeyEnvironmentVariable' is not set"
    }

    $script:BaseImmichUrl = Get-BaseImmichUrl
    $script:ApiHeaders = @{ 'x-api-key' = $apiKey }
    $apiKey = $null

    $working = @{}
    foreach ($record in $existingRecords) {
        $key = Get-RecordKey $record.owner_id $record.source_sha1
        if ($working.ContainsKey($key)) {
            throw "Existing ledger contains a duplicate source key for owner $($record.owner_id)"
        }
        $working[$key] = $record
    }

    $authenticatedOwner = Get-AuthenticatedOwner
    $assetIDs = @(Get-AllAssetIDs)
    $candidates = @{}
    $candidateAssetKeys = @{}

    foreach ($assetID in $assetIDs) {
        $entry = Get-ProvenanceEntry -AssetID $assetID
        if ($null -eq $entry) {
            continue
        }
        $candidate = ConvertTo-ProvenanceCandidate -AssetID $assetID -Entry $entry -AuthenticatedOwner $authenticatedOwner
        $key = Get-RecordKey $candidate.OwnerID $candidate.SourceSHA1

        if ($candidateAssetKeys.ContainsKey($assetID)) {
            $priorKey = $candidateAssetKeys[$assetID]
            if ($priorKey -cne $key) {
                throw "Asset $assetID maps to multiple source keys; refusing ambiguous recovery"
            }
        }
        $candidateAssetKeys[$assetID] = $key

        if ($candidates.ContainsKey($key)) {
            $prior = $candidates[$key]
            if ($prior.AssetID -cne $assetID) {
                throw "Source key for owner $($candidate.OwnerID) maps to multiple asset ids; refusing ambiguous recovery"
            }
            continue
        }
        $candidates[$key] = $candidate
    }

    if ($candidates.Count -eq 0) {
        throw 'No provenance records were found for the authenticated owner; refusing to write an empty ledger'
    }

    foreach ($key in @($candidates.Keys | Sort-Object)) {
        $candidate = $candidates[$key]
        $remote = Get-RemoteOriginalInfo -AssetID $candidate.AssetID
        if ($remote.SHA1 -cne $candidate.OutputSHA1) {
            throw "Remote output SHA-1 mismatch for asset $($candidate.AssetID); record remains unverified"
        }
        if ($remote.Size -ne $candidate.OutputSize) {
            throw "Remote output size mismatch for asset $($candidate.AssetID); record remains unverified"
        }

        $working[$key] = Merge-VerifiedRecord -Existing $working[$key] -Recovered $candidate
    }

    # One remote asset id can only identify one source record. Alias collisions
    # would make FindAsset ambiguous in the Go store.
    $assetIndex = @{}
    foreach ($record in $working.Values) {
        if ([string]::IsNullOrEmpty($record.asset_id)) {
            continue
        }
        if ($assetIndex.ContainsKey($record.asset_id) -and $assetIndex[$record.asset_id] -cne (Get-RecordKey $record.owner_id $record.source_sha1)) {
            throw "Asset alias $($record.asset_id) maps to multiple source records; refusing to write"
        }
        $assetIndex[$record.asset_id] = Get-RecordKey $record.owner_id $record.source_sha1
    }

    Assert-OptimizerStopped
    $allRecords = @($working.Values)
    Write-AtomicLedger -Path $ledgerPath -Records $allRecords

    $writtenRecords = @(Read-ExistingLedger -Path $ledgerPath)
    if ($writtenRecords.Count -ne $allRecords.Count) {
        throw 'Post-write ledger validation found an unexpected record count'
    }

    Write-Output "Rebuilt ledger with $($writtenRecords.Count) record(s) at $ledgerPath"
}

if ($MyInvocation.InvocationName -ne '.') {
    Invoke-Rebuild
}
